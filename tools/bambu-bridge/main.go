// Command bambu-bridge republishes a Bambu Lab printer's status into
// maskinsjekk as an ordinary sensor, so the printer's own idea of what it is
// doing (idle, preparing, printing, finished, failed) is stored next to the
// vibration/temperature data and can serve as ground-truth labels.
//
// It connects to the printer's local MQTT server (TLS on 8883, user "bblp",
// the LAN access code from the printer's screen), subscribes to
// device/<serial>/report, asks for a full status ("pushall") and merges the
// partial reports that follow. Every INTERVAL, and immediately when the
// state, print stage or door changes, it publishes numeric fields to TOPIC on
// the maskinsjekk broker, e.g.
//
//	{"ts":..., "printer_reachable":1, "state_code":2, "active":1, "stage":0,
//	 "percent":42, "layer":120, "nozzle_c":220.1, "bed_c":65, "door_open":0}
//
// While the printer can't be reached only {"ts":..., "printer_reachable":0} is
// sent. Printer-specific code lives here, outside the generic server.
//
// Configuration (environment):
//
//	BAMBU_SERIAL       printer serial number (required)
//	BAMBU_ACCESS_CODE  LAN access code (required; never commit it)
//	BAMBU_HOST         printer IP; empty = find it by its UDP 2021 announcements
//	MQTT_URL           maskinsjekk broker (default tcp://localhost:1883)
//	TOPIC              default hsd/demo/printer-1/bambu
//	INTERVAL           publish interval (default 5s)
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

// reportTimeout: a connected printer that stays silent this long is treated as
// unreachable and the connection is restarted (rediscovering its address).
const reportTimeout = 90 * time.Second

var topicRe = regexp.MustCompile(`^hsd/[a-z0-9_-]{1,64}/[a-z0-9_-]{1,64}/[a-z0-9_-]{1,64}$`)

type config struct {
	serial, accessCode, host string
	mqttURL, topic           string
	interval                 time.Duration
}

func loadConfig() (config, error) {
	c := config{
		serial:     os.Getenv("BAMBU_SERIAL"),
		accessCode: os.Getenv("BAMBU_ACCESS_CODE"),
		host:       os.Getenv("BAMBU_HOST"),
		mqttURL:    envOr("MQTT_URL", "tcp://localhost:1883"),
		topic:      envOr("TOPIC", "hsd/demo/printer-1/bambu"),
		interval:   5 * time.Second,
	}
	if c.serial == "" || c.accessCode == "" {
		return c, errors.New("BAMBU_SERIAL and BAMBU_ACCESS_CODE must be set")
	}
	if !topicRe.MatchString(c.topic) {
		return c, fmt.Errorf("TOPIC %q is not hsd/<site>/<machine>/<sensor> with ids of [a-z0-9_-]", c.topic)
	}
	if s := os.Getenv("INTERVAL"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d < time.Second {
			return c, fmt.Errorf("INTERVAL %q: want a duration of at least 1s", s)
		}
		c.interval = d
	}
	return c, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// bridge holds the merged printer state; printer callbacks write it, the
// publisher reads it.
type bridge struct {
	mu         sync.Mutex
	state      map[string]any
	connected  bool
	lastReport time.Time
	changed    chan struct{} // nudges the publisher on a state transition
}

func newBridge() *bridge {
	return &bridge{state: map[string]any{}, changed: make(chan struct{}, 1)}
}

func (b *bridge) setConnected(up bool) {
	b.mu.Lock()
	b.connected = up
	if up {
		b.state = map[string]any{} // a fresh pushall follows; drop the old session's fields
		b.lastReport = time.Time{}
	}
	b.mu.Unlock()
	if !up {
		b.nudge() // report the loss now; on connect the first report nudges
	}
}

func (b *bridge) handleReport(payload []byte) {
	b.mu.Lock()
	before := changeKey(b.state)
	ok := mergeReport(b.state, payload)
	if ok {
		b.lastReport = time.Now()
	}
	changed := ok && changeKey(b.state) != before
	b.mu.Unlock()
	if changed {
		b.nudge()
	}
}

func (b *bridge) nudge() {
	select {
	case b.changed <- struct{}{}:
	default:
	}
}

func (b *bridge) silentFor(now time.Time) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lastReport.IsZero() {
		return 0
	}
	return now.Sub(b.lastReport)
}

func (b *bridge) payload(now time.Time) []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	reachable := b.connected && !b.lastReport.IsZero() && now.Sub(b.lastReport) < reportTimeout
	return buildPayload(b.state, reachable, now.UnixMilli())
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("bambu-bridge: ")
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	b := newBridge()
	out := connectBroker(cfg)
	defer out.Disconnect(250)
	go publishLoop(ctx, cfg, b, out)

	backoff := 5 * time.Second
	for ctx.Err() == nil {
		host := cfg.host
		if host == "" {
			if host, err = discover(ctx, cfg.serial, 30*time.Second); err != nil {
				log.Printf("discover: %v", err)
				sleep(ctx, backoff)
				continue
			}
			log.Printf("printer %s found at %s", cfg.serial, host)
		}
		if err := runPrinterSession(ctx, cfg, host, b); err != nil && ctx.Err() == nil {
			log.Printf("printer %s: %v", host, err)
		}
		b.setConnected(false)
		sleep(ctx, backoff)
	}
}

// connectBroker connects to the maskinsjekk broker; paho keeps reconnecting on
// its own. The retained <topic>/status mirrors the ESP32 sensors.
func connectBroker(cfg config) mqtt.Client {
	status := cfg.topic + "/status"
	opts := mqtt.NewClientOptions().
		AddBroker(cfg.mqttURL).
		SetClientID("maskinsjekk-bambu-bridge").
		SetWill(status, "offline", 1, true).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(5 * time.Second).
		SetOnConnectHandler(func(c mqtt.Client) {
			c.Publish(status, 1, true, "online")
			log.Printf("connected to %s, publishing to %s", cfg.mqttURL, cfg.topic)
		})
	c := mqtt.NewClient(opts)
	c.Connect() // with ConnectRetry this keeps trying in the background
	return c
}

func publishLoop(ctx context.Context, cfg config, b *bridge, out mqtt.Client) {
	t := time.NewTicker(cfg.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-b.changed:
		}
		if out.IsConnectionOpen() {
			out.Publish(cfg.topic, 0, false, b.payload(time.Now()))
		}
	}
}

// runPrinterSession connects to the printer and blocks until the connection is
// lost, the printer goes silent, or ctx ends.
func runPrinterSession(ctx context.Context, cfg config, host string, b *bridge) error {
	reportTopic := "device/" + cfg.serial + "/report"
	requestTopic := "device/" + cfg.serial + "/request"
	lost := make(chan error, 1)

	opts := mqtt.NewClientOptions().
		AddBroker("tls://" + host + ":8883").
		SetClientID("maskinsjekk-bambu-bridge").
		SetUsername("bblp").
		SetPassword(cfg.accessCode).
		// The printer's certificate is signed by Bambu's private CA for its
		// serial number, not its IP, so it can't be verified normally. The
		// traffic stays on the local network.
		SetTLSConfig(&tls.Config{InsecureSkipVerify: true}).
		SetAutoReconnect(false).
		SetConnectTimeout(10 * time.Second).
		SetKeepAlive(30 * time.Second).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			select {
			case lost <- err:
			default:
			}
		})
	c := mqtt.NewClient(opts)
	if tok := c.Connect(); !tok.WaitTimeout(15*time.Second) || tok.Error() != nil {
		if tok.Error() != nil {
			return fmt.Errorf("connect: %w (check BAMBU_ACCESS_CODE)", tok.Error())
		}
		return errors.New("connect: timed out")
	}
	defer c.Disconnect(250)

	b.setConnected(true)
	if tok := c.Subscribe(reportTopic, 0, func(_ mqtt.Client, m mqtt.Message) { b.handleReport(m.Payload()) }); tok.WaitTimeout(10*time.Second) && tok.Error() != nil {
		return fmt.Errorf("subscribe: %w", tok.Error())
	}
	c.Publish(requestTopic, 0, false, `{"pushing":{"sequence_id":"0","command":"pushall"}}`)
	log.Printf("connected to printer %s at %s", cfg.serial, host)

	watchdog := time.NewTicker(10 * time.Second)
	defer watchdog.Stop()
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-lost:
			return fmt.Errorf("connection lost: %w", err)
		case now := <-watchdog.C:
			silent := b.silentFor(now)
			if silent == 0 {
				silent = now.Sub(start) // nothing received at all yet
			}
			if silent > reportTimeout {
				return fmt.Errorf("no report for %s (wrong BAMBU_SERIAL?)", silent.Round(time.Second))
			}
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
