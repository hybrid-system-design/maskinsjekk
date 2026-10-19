// Package ingest subscribes to the MQTT broker and normalizes every message into
// measurements (machine_id, sensor_id, metric, value, ts). It is deliberately
// resilient: bad payloads are logged and dropped, never fatal, and the client
// auto-reconnects (re-subscribing on every connect).
//
// Sensor topic:   hsd/<site>/<machine>/<sensor>
// Sensor payload: {"ts": <unix_ms>, "<metric>": <number>, ...}
//
// Every numeric field other than "ts" becomes a metric, so a new sensor type
// needs no server change (e.g. {"ts":..., "vib_rms":0.012, "temp_c":24.1}).
package ingest

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"

	"maskinsjekk/internal/config"
	"maskinsjekk/internal/store"
)

// maxMetricsPerMessage bounds what one message can write, so a buggy or hostile
// publisher can't flood the measurements table with junk metric names.
const maxMetricsPerMessage = 16

// Ingestor owns the MQTT client and feeds measurements into the store.
type Ingestor struct {
	cfg    config.Config
	store  *store.Store
	log    *log.Logger
	client mqtt.Client
}

// New constructs an Ingestor.
func New(cfg config.Config, st *store.Store, logger *log.Logger) *Ingestor {
	return &Ingestor{cfg: cfg, store: st, log: logger}
}

// Start connects to the broker and subscribes. With connect-retry enabled it
// returns without error even if the broker is not yet reachable; the client
// keeps retrying and (re)subscribes via the OnConnect handler.
func (in *Ingestor) Start() error {
	opts := mqtt.NewClientOptions().
		AddBroker(in.cfg.MQTTURL).
		SetClientID(in.cfg.MQTTClientID).
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(in.cfg.MQTTConnectRetryInterval).
		SetMaxReconnectInterval(in.cfg.MQTTMaxReconnectInterval).
		SetOnConnectHandler(func(c mqtt.Client) {
			in.log.Printf("mqtt: connected, subscribing to %q", in.cfg.MQTTTopic)
			// CleanSession drops subscriptions on reconnect, so re-subscribe
			// every time we (re)connect.
			t := c.Subscribe(in.cfg.MQTTTopic, 1, in.handle)
			if t.Wait() && t.Error() != nil {
				in.log.Printf("mqtt: subscribe error: %v", t.Error())
			}
		}).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			in.log.Printf("mqtt: connection lost: %v", err)
		})

	in.client = mqtt.NewClient(opts)
	tok := in.client.Connect()
	// Don't block startup if the broker is slow/down: connect-retry handles it.
	if tok.WaitTimeout(10*time.Second) && tok.Error() != nil {
		in.log.Printf("mqtt: initial connect error (will keep retrying): %v", tok.Error())
	}
	return nil
}

// Stop disconnects the client cleanly.
func (in *Ingestor) Stop() {
	if in.client != nil && in.client.IsConnectionOpen() {
		in.client.Disconnect(250)
	} else if in.client != nil {
		in.client.Disconnect(0)
	}
}

// handle validates and stores one incoming message. It never panics or returns
// errors upward; ingestion must survive any malformed input.
func (in *Ingestor) handle(_ mqtt.Client, msg mqtt.Message) {
	now := time.Now()
	ms, reason, ok := parseSensorMessage(msg.Topic(), msg.Payload(), now, in.cfg.SanityPast, in.cfg.SanityFuture)
	if !ok {
		in.log.Printf("ingest: drop message on %q: %s", msg.Topic(), reason)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if !in.cfg.AutoRegister {
		known, err := in.store.MachineExists(ctx, ms[0].MachineID)
		if err != nil {
			in.log.Printf("ingest: lookup machine %s: %v", ms[0].MachineID, err)
			return
		}
		if !known {
			in.log.Printf("ingest: drop message on %q: unknown machine (AUTO_REGISTER=false)", msg.Topic())
			return
		}
	}
	if err := in.store.InsertMeasurements(ctx, ms); err != nil {
		in.log.Printf("ingest: store insert failed for %s: %v", msg.Topic(), err)
	}
}

// parseSensorMessage validates and normalizes a raw sensor message into
// measurements. ok is false (with a human reason) when the message is unusable,
// in which case callers log-and-drop. It is pure (no I/O), so it can be unit-
// and fuzz-tested directly — this is the wire-format trust boundary.
func parseSensorMessage(topic string, raw []byte, now time.Time, past, future time.Duration) ([]store.Measurement, string, bool) {
	site, machine, sensor, ok := parseTopic(topic)
	if !ok {
		return nil, "topic is not hsd/<site>/<machine>/<sensor> with valid ids", false
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil || fields == nil {
		return nil, "bad json (want an object)", false
	}

	var (
		tsMS  int64
		hasTS bool
	)
	if n, isNum := fields["ts"].(json.Number); isNum {
		if v, err := n.Int64(); err == nil {
			tsMS, hasTS = v, true
		} else if f, err := n.Float64(); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) && math.Abs(f) < 1e15 {
			tsMS, hasTS = int64(f), true
		}
	}
	nowMS := now.UnixMilli()
	ts := resolveEventTime(tsMS, hasTS, nowMS, past, future)

	// Deterministic order keeps tests and the per-message cap stable.
	keys := make([]string, 0, len(fields))
	for k := range fields {
		if k != "ts" {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var out []store.Measurement
	for _, k := range keys {
		n, isNum := fields[k].(json.Number)
		if !isNum || !config.ValidID(k) {
			continue // strings, nested objects, odd names: not a metric
		}
		v, err := n.Float64()
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		if len(out) == maxMetricsPerMessage {
			break
		}
		out = append(out, store.Measurement{
			SiteID: site, MachineID: machine, SensorID: sensor,
			Metric: k, Value: v, TS: ts, ReceivedAt: nowMS,
		})
	}
	if len(out) == 0 {
		return nil, "no numeric metrics", false
	}
	return out, "", true
}

// parseTopic splits "hsd/<site>/<machine>/<sensor>" and validates each id.
func parseTopic(topic string) (site, machine, sensor string, ok bool) {
	parts := strings.Split(topic, "/")
	if len(parts) != 4 || parts[0] != "hsd" {
		return "", "", "", false
	}
	for _, p := range parts[1:] {
		if !config.ValidID(p) {
			return "", "", "", false
		}
	}
	return parts[1], parts[2], parts[3], true
}

// resolveEventTime returns the canonical series time (unix ms). It honors a
// device-reported timestamp only when it falls within [now-past, now+future];
// otherwise (absent, or wildly off because NTP hasn't synced yet) it falls back
// to the server arrival time. This lets a simulator backfill history while
// rejecting 1970/far-future timestamps from an unsynced device.
func resolveEventTime(tsMS int64, hasTS bool, nowMS int64, past, future time.Duration) int64 {
	if hasTS {
		lower := nowMS - past.Milliseconds()
		upper := nowMS + future.Milliseconds()
		if tsMS >= lower && tsMS <= upper {
			return tsMS
		}
	}
	return nowMS
}
