package ingest

import (
	"math"
	"testing"
)

// FuzzParseSensorMessage hammers the wire-format trust boundary: whatever arrives
// on the broker must never panic, and anything accepted must be storable.
func FuzzParseSensorMessage(f *testing.F) {
	seeds := []struct{ topic, payload string }{
		{"hsd/demo/printer-1/esp32-1", `{"ts":1792411200000,"vib_rms":0.0123,"temp_c":24.5}`},
		{"hsd/demo/printer-1/esp32-1", `{"vib_rms":1e308}`},
		{"hsd/demo/printer-1/esp32-1", `{"ts":1e400,"vib_rms":1}`},
		{"hsd/demo/printer-1/esp32-1", `{"ts":"now","vib_rms":-0}`},
		{"hsd/a/b/c", `{}`},
		{"hsd/a/b/c", ``},
		{"", `{"x":1}`},
	}
	for _, s := range seeds {
		f.Add(s.topic, []byte(s.payload))
	}
	f.Fuzz(func(t *testing.T, topic string, raw []byte) {
		ms, reason, ok := parseSensorMessage(topic, raw, testNow, testPast, testFuture)
		if !ok {
			if reason == "" {
				t.Fatalf("drop without reason")
			}
			return
		}
		if len(ms) == 0 || len(ms) > maxMetricsPerMessage {
			t.Fatalf("accepted %d measurements", len(ms))
		}
		lower := testNow.Add(-testPast).UnixMilli()
		upper := testNow.Add(testFuture).UnixMilli()
		for _, m := range ms {
			if m.MachineID == "" || m.SensorID == "" || m.SiteID == "" || m.Metric == "" {
				t.Fatalf("accepted measurement with empty id: %+v", m)
			}
			if math.IsNaN(m.Value) || math.IsInf(m.Value, 0) {
				t.Fatalf("accepted non-finite value: %+v", m)
			}
			if m.TS < lower || m.TS > upper {
				t.Fatalf("ts outside sanity window: %d", m.TS)
			}
		}
	})
}
