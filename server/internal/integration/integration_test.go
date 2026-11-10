//go:build integration

// Package integration exercises the full ingest path against a real Mosquitto
// broker (started via testcontainers): MQTT publish -> subscribe -> SQLite ->
// HTTP. Run with: go test -tags integration ./internal/integration/...
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"maskinsjekk/internal/api"
	"maskinsjekk/internal/config"
	"maskinsjekk/internal/ingest"
	"maskinsjekk/internal/store"
)

// --- response shapes (mirror the api package's JSON contract) ---

type point struct {
	TS  int64   `json:"ts"`
	Avg float64 `json:"avg"`
	N   int     `json:"n"`
}

type seriesResp struct {
	Points []point `json:"points"`
}

type machinesResp struct {
	Machines []struct {
		ID     string `json:"id"`
		SiteID string `json:"site_id"`
		Stale  bool   `json:"stale"`
	} `json:"machines"`
}

// TestPipeline covers the success path plus bad-payload resilience against a
// single shared broker. Each subtest uses its own machine id for isolation.
func TestPipeline(t *testing.T) {
	hostPort := startBroker(t)
	brokerURL := "tcp://" + hostPort
	baseURL := startService(t, brokerURL, true)
	pub := newPublisher(t, brokerURL)

	t.Run("round_trip", func(t *testing.T) {
		pub.send(t, "rt-1", map[string]float64{"vib_rms": 0.05, "temp_c": 24.5}, time.Now())
		waitForMetric(t, baseURL, "rt-1", "vib_rms", 0.05, 10*time.Second)
		waitForMetric(t, baseURL, "rt-1", "temp_c", 24.5, 10*time.Second)

		var m machinesResp
		getJSON(t, baseURL+"/machines", &m)
		found := false
		for _, mc := range m.Machines {
			if mc.ID == "rt-1" {
				found = true
				if mc.SiteID != "itest" || mc.Stale {
					t.Fatalf("unexpected machine: %+v", mc)
				}
			}
		}
		if !found {
			t.Fatalf("rt-1 not auto-registered: %+v", m)
		}
	})

	t.Run("backfill_honors_device_ts", func(t *testing.T) {
		old := time.Now().Add(-3 * time.Hour)
		pub.send(t, "bf-1", map[string]float64{"vib_rms": 0.2}, old)
		var s seriesResp
		waitFor(t, 10*time.Second, func() bool {
			if !tryGetJSON(seriesURL(baseURL, "bf-1", "vib_rms"), &s) || len(s.Points) == 0 {
				return false
			}
			return true
		})
		if d := time.Since(time.UnixMilli(s.Points[0].TS)); d < 2*time.Hour {
			t.Fatalf("device ts not honored: bucket %s ago", d)
		}
	})

	t.Run("bad_payload_resilience", func(t *testing.T) {
		// Garbage on the topic must not kill ingestion: a valid message published
		// right after still lands.
		pub.raw(t, "hsd/itest/bad-1/esp32-1", []byte("}{ not json"))
		pub.raw(t, "hsd/itest/bad-1/esp32-1", []byte(`{"fw":"1.0"}`)) // no metrics
		pub.send(t, "bad-1", map[string]float64{"vib_rms": 0.7}, time.Now())
		waitForMetric(t, baseURL, "bad-1", "vib_rms", 0.7, 10*time.Second)
	})

	t.Run("shelly_status", func(t *testing.T) {
		// Native Shelly Plug S Gen3 status, mapped to plug-1/shelly-1 by the
		// machines file.
		pub.raw(t, "itest-shelly/status/switch:0", []byte(`{"id":0, "source":"init", "output":true, "apower":182.5, "voltage":231.2, "freq":50.0, "current":0.810, "aenergy":{"total":13.000,"by_minute":[0.000,0.000,0.000],"minute_ts":1794317880}, "ret_aenergy":{"total":0.000,"by_minute":[0.000,0.000,0.000],"minute_ts":1794317880},"temperature":{"tC":35.8, "tF":96.5}}`))
		waitForMetric(t, baseURL, "plug-1", "power_w", 182.5, 10*time.Second)
		waitForMetric(t, baseURL, "plug-1", "energy_wh", 13, 10*time.Second)
		waitForMetric(t, baseURL, "plug-1", "output", 1, 10*time.Second)
	})
}

// TestAutoRegisterOff drops messages for machines not in the machines file.
func TestAutoRegisterOff(t *testing.T) {
	hostPort := startBroker(t)
	baseURL := startService(t, "tcp://"+hostPort, false)
	pub := newPublisher(t, "tcp://"+hostPort)

	// "known-1" is pre-registered by startService; "stranger" is not.
	pub.send(t, "stranger", map[string]float64{"vib_rms": 1}, time.Now())
	pub.send(t, "known-1", map[string]float64{"vib_rms": 1}, time.Now())
	waitForMetric(t, baseURL, "known-1", "vib_rms", 1, 10*time.Second)

	resp, err := http.Get(seriesURL(baseURL, "stranger", "vib_rms"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown machine was stored (status %d)", resp.StatusCode)
	}
}

// TestReconnectAfterBrokerBounce proves the subscriber re-establishes after its
// connection to the broker drops and returns. The service connects through an
// in-process TCP proxy; we drop and restore the proxy (broker stays up).
func TestReconnectAfterBrokerBounce(t *testing.T) {
	hostPort := startBroker(t)
	proxy := newProxy(t, hostPort)

	baseURL := startService(t, "tcp://"+proxy.addr(), true)
	pub := newPublisher(t, "tcp://"+hostPort)

	pub.send(t, "rc-1", map[string]float64{"vib_rms": 1}, time.Now())
	waitForMetric(t, baseURL, "rc-1", "vib_rms", 1, 10*time.Second)

	proxy.stop()
	proxy.start()

	// QoS1/clean-session means messages sent while disconnected are not retained,
	// so keep publishing (to a machine not seen before the bounce) until one lands.
	waitFor(t, 30*time.Second, func() bool {
		pub.send(t, "rc-after", map[string]float64{"vib_rms": 9}, time.Now())
		var s seriesResp
		return tryGetJSON(seriesURL(baseURL, "rc-after", "vib_rms"), &s) && len(s.Points) > 0
	})
}

// --- broker / service harness ---

func startBroker(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	req := testcontainers.ContainerRequest{
		Image:        "eclipse-mosquitto:2",
		ExposedPorts: []string{"1883/tcp"},
		Files: []testcontainers.ContainerFile{{
			HostFilePath:      writeMosquittoConf(t),
			ContainerFilePath: "/mosquitto/config/mosquitto.conf",
			FileMode:          0o644,
		}},
		WaitingFor: wait.ForListeningPort("1883/tcp").WithStartupTimeout(60 * time.Second),
	}
	c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		t.Fatalf("start mosquitto: %v", err)
	}
	t.Cleanup(func() { _ = c.Terminate(context.Background()) })

	host, err := c.Host(ctx)
	if err != nil {
		t.Fatalf("broker host: %v", err)
	}
	mapped, err := c.MappedPort(ctx, "1883/tcp")
	if err != nil {
		t.Fatalf("broker port: %v", err)
	}
	return fmt.Sprintf("%s:%s", host, mapped.Port())
}

// startService boots the store + HTTP API + MQTT ingestor in-process and returns
// the API base URL. Machine "known-1" is always pre-registered, as is "plug-1"
// with a Shelly sensor on prefix "itest-shelly".
func startService(t *testing.T, brokerURL string, autoRegister bool) string {
	t.Helper()
	cfg := config.Config{
		MQTTURL:                  brokerURL,
		MQTTTopic:                "hsd/+/+/+",
		MQTTClientID:             "maskinsjekk-itest-" + randSuffix(),
		AutoRegister:             autoRegister,
		StaleThreshold:           30 * time.Second,
		SanityPast:               50 * time.Hour,
		SanityFuture:             5 * time.Minute,
		MQTTConnectRetryInterval: 500 * time.Millisecond,
		MQTTMaxReconnectInterval: 1 * time.Second,
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "it.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	machines := config.Machines{Sites: []config.Site{{
		ID: "itest", Machines: []config.Machine{
			{ID: "known-1"},
			{ID: "plug-1", Sensors: []config.Sensor{{ID: "shelly-1", Kind: config.KindShellyGen2, MQTTPrefix: "itest-shelly"}}},
		},
	}}}
	if err := st.SyncRegistry(context.Background(), machines); err != nil {
		t.Fatalf("SyncRegistry: %v", err)
	}
	shelly := machines.ShellyDevices()

	logger := log.New(io.Discard, "", 0)
	ts := httptest.NewServer(api.New(st, cfg, logger).Handler())
	t.Cleanup(ts.Close)

	ing := ingest.New(cfg, st, shelly, logger)
	if err := ing.Start(); err != nil {
		t.Fatalf("ingest.Start: %v", err)
	}
	t.Cleanup(ing.Stop)
	return ts.URL
}

// --- publisher ---

type publisher struct{ client mqtt.Client }

func newPublisher(t *testing.T, brokerURL string) *publisher {
	t.Helper()
	opts := mqtt.NewClientOptions().
		AddBroker(brokerURL).
		SetClientID("itest-pub-" + randSuffix()).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(300 * time.Millisecond)
	c := mqtt.NewClient(opts)
	tok := c.Connect()
	if !tok.WaitTimeout(10*time.Second) || tok.Error() != nil {
		t.Fatalf("publisher connect: %v", tok.Error())
	}
	t.Cleanup(func() { c.Disconnect(100) })
	return &publisher{client: c}
}

// send publishes an ESP32-shaped message for machine on site "itest".
func (p *publisher) send(t *testing.T, machine string, metrics map[string]float64, ts time.Time) {
	t.Helper()
	body := map[string]any{"ts": ts.UnixMilli()}
	for k, v := range metrics {
		body[k] = v
	}
	raw, _ := json.Marshal(body)
	p.raw(t, "hsd/itest/"+machine+"/esp32-1", raw)
}

func (p *publisher) raw(t *testing.T, topic string, body []byte) {
	t.Helper()
	tok := p.client.Publish(topic, 1, false, body)
	if !tok.WaitTimeout(5*time.Second) || tok.Error() != nil {
		// During a blip the publisher may be mid-reconnect; let the caller's
		// retry loop handle it rather than failing hard.
		t.Logf("publish to %q pending/failed: %v", topic, tok.Error())
	}
}

// --- in-process TCP proxy (controllable broker link) ---

type tcpProxy struct {
	t      *testing.T
	target string
	port   string

	mu    sync.Mutex
	ln    net.Listener
	conns map[net.Conn]struct{}
}

func newProxy(t *testing.T, target string) *tcpProxy {
	t.Helper()
	p := &tcpProxy{t: t, target: target, conns: map[net.Conn]struct{}{}}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	p.port = port
	p.ln = l
	go p.acceptLoop(l)
	t.Cleanup(p.stop)
	return p
}

func (p *tcpProxy) addr() string { return "127.0.0.1:" + p.port }

func (p *tcpProxy) acceptLoop(l net.Listener) {
	for {
		client, err := l.Accept()
		if err != nil {
			return // listener closed
		}
		go p.pipe(client)
	}
}

func (p *tcpProxy) pipe(client net.Conn) {
	upstream, err := net.Dial("tcp", p.target)
	if err != nil {
		client.Close()
		return
	}
	p.track(client)
	p.track(upstream)
	go func() { _, _ = io.Copy(upstream, client); upstream.Close() }()
	_, _ = io.Copy(client, upstream)
	client.Close()
}

func (p *tcpProxy) track(c net.Conn) {
	p.mu.Lock()
	p.conns[c] = struct{}{}
	p.mu.Unlock()
}

// stop closes the listener and all live connections, severing the service link.
func (p *tcpProxy) stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ln != nil {
		p.ln.Close()
		p.ln = nil
	}
	for c := range p.conns {
		c.Close()
		delete(p.conns, c)
	}
}

// start re-listens on the same port.
func (p *tcpProxy) start() {
	l, err := net.Listen("tcp", p.addr())
	if err != nil {
		p.t.Fatalf("proxy relisten: %v", err)
	}
	p.mu.Lock()
	p.ln = l
	p.mu.Unlock()
	go p.acceptLoop(l)
}

// --- polling / assertion helpers ---

func seriesURL(baseURL, machine, metric string) string {
	from := time.Now().Add(-6 * time.Hour).UnixMilli()
	to := time.Now().Add(time.Minute).UnixMilli()
	return fmt.Sprintf("%s/machines/%s/series?metric=%s&from=%d&to=%d", baseURL, machine, metric, from, to)
}

// waitForMetric waits until the machine's series contains a bucket averaging want.
func waitForMetric(t *testing.T, baseURL, machine, metric string, want float64, timeout time.Duration) {
	t.Helper()
	waitFor(t, timeout, func() bool {
		var s seriesResp
		if !tryGetJSON(seriesURL(baseURL, machine, metric), &s) {
			return false
		}
		for _, p := range s.Points {
			if p.Avg == want {
				return true
			}
		}
		return false
	})
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	if !tryGetJSON(url, out) {
		t.Fatalf("GET %s: no 200/JSON", url)
	}
}

func tryGetJSON(url string, out any) bool {
	resp, err := http.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	return json.NewDecoder(resp.Body).Decode(out) == nil
}

func writeMosquittoConf(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mosquitto.conf")
	conf := "listener 1883\nallow_anonymous true\npersistence false\n"
	if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
		t.Fatalf("write mosquitto.conf: %v", err)
	}
	return path
}

func randSuffix() string { return fmt.Sprintf("%d", time.Now().UnixNano()) }
