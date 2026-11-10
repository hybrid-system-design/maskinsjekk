package ingest

import (
	"math"
	"testing"

	"maskinsjekk/internal/config"
)

var testShelly = config.ShellyDevice{Prefix: "shelly-printer-1", SiteID: "demo", MachineID: "printer-1", SensorID: "shelly-1"}

// Captured from a real Shelly Plug S Gen3 (shelly-printer-1/status/switch:0).
const plugSGen3Status = `{"id":0, "source":"init", "output":false, "apower":0.0, "voltage":231.2, "freq":50.0, "current":0.000, "aenergy":{"total":13.000,"by_minute":[0.000,0.000,0.000],"minute_ts":1794317880}, "ret_aenergy":{"total":0.000,"by_minute":[0.000,0.000,0.000],"minute_ts":1794317880},"temperature":{"tC":35.8, "tF":96.5}}`

func TestParseShellyStatus(t *testing.T) {
	nowMS := testNow.UnixMilli()

	t.Run("plug s gen3 switch status", func(t *testing.T) {
		ms, reason, ok := parseShellyStatus([]byte(plugSGen3Status), testShelly, testNow)
		if !ok {
			t.Fatalf("dropped: %s", reason)
		}
		got := map[string]float64{}
		for _, m := range ms {
			if m.SiteID != "demo" || m.MachineID != "printer-1" || m.SensorID != "shelly-1" {
				t.Fatalf("ids not from mapping: %+v", m)
			}
			if m.TS != nowMS || m.ReceivedAt != nowMS {
				t.Fatalf("want arrival time, got ts=%d recv=%d", m.TS, m.ReceivedAt)
			}
			got[m.Metric] = m.Value
		}
		want := map[string]float64{
			"power_w": 0, "voltage_v": 231.2, "current_a": 0, "freq_hz": 50,
			"energy_wh": 13, "device_temp_c": 35.8, "output": 0,
		}
		if len(got) != len(want) {
			t.Fatalf("got metrics %v, want %v", got, want)
		}
		for k, v := range want {
			if g, ok := got[k]; !ok || g != v {
				t.Errorf("%s = %v (present %v), want %v", k, g, ok, v)
			}
		}
	})

	t.Run("relay on -> output 1", func(t *testing.T) {
		ms, _, ok := parseShellyStatus([]byte(`{"output":true,"apower":182.5}`), testShelly, testNow)
		if !ok || len(ms) != 2 || ms[0].Metric != "power_w" || ms[0].Value != 182.5 || ms[1].Metric != "output" || ms[1].Value != 1 {
			t.Fatalf("ok=%v ms=%+v", ok, ms)
		}
	})

	t.Run("pm1 meter without relay or temperature", func(t *testing.T) {
		ms, _, ok := parseShellyStatus([]byte(`{"id":0,"voltage":230.1,"current":2.1,"apower":480.2,"pf":0.98,"freq":50,"aenergy":{"total":5321.7}}`), testShelly, testNow)
		if !ok || len(ms) != 6 {
			t.Fatalf("ok=%v ms=%+v", ok, ms)
		}
		for _, m := range ms {
			if m.Metric == "output" || m.Metric == "device_temp_c" {
				t.Fatalf("absent field stored: %+v", m)
			}
		}
	})

	drops := map[string]string{
		"bad json":          `}{`,
		"array":             `[1]`,
		"null":              `null`,
		"empty object":      `{}`,
		"unrelated status":  `{"uptime":12,"ram_free":1000}`,
		"wrong type":        `{"apower":"182"}`,
		"number overflow":   `{"apower":1e400}`,
		"nested wrong type": `{"aenergy":{"total":"x"}}`,
	}
	for name, payload := range drops {
		t.Run("drop: "+name, func(t *testing.T) {
			if _, reason, ok := parseShellyStatus([]byte(payload), testShelly, testNow); ok || reason == "" {
				t.Fatalf("ok=%v reason=%q", ok, reason)
			}
		})
	}
}

func TestShellyTopics(t *testing.T) {
	got := shellyTopics(config.ShellyDevice{Prefix: "site/plug-a", Channel: 2})
	if len(got) != 2 || got[0] != "site/plug-a/status/switch:2" || got[1] != "site/plug-a/status/pm1:2" {
		t.Fatalf("topics = %v", got)
	}
}

// FuzzParseShellyStatus: arbitrary payloads on a Shelly topic must never panic,
// and anything accepted must be storable under the mapped ids.
func FuzzParseShellyStatus(f *testing.F) {
	for _, s := range []string{plugSGen3Status, `{"output":true}`, `{"apower":1e308}`, `{"aenergy":null}`, `{}`, ``} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		ms, reason, ok := parseShellyStatus(raw, testShelly, testNow)
		if !ok {
			if reason == "" {
				t.Fatalf("drop without reason")
			}
			return
		}
		if len(ms) == 0 || len(ms) > maxMetricsPerMessage {
			t.Fatalf("accepted %d measurements", len(ms))
		}
		for _, m := range ms {
			if !config.ValidID(m.Metric) || m.MachineID != "printer-1" || m.SensorID != "shelly-1" {
				t.Fatalf("bad measurement: %+v", m)
			}
			if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) {
				t.Fatalf("accepted non-finite value: %+v", m)
			}
		}
	})
}
