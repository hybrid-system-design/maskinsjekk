package ingest

import (
	"strconv"
	"testing"
	"time"
)

var (
	testNow    = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	testPast   = 50 * time.Hour
	testFuture = 5 * time.Minute
)

func TestParseSensorMessage(t *testing.T) {
	nowMS := testNow.UnixMilli()
	tsMS := testNow.Add(-2 * time.Second).UnixMilli()

	t.Run("esp32 payload -> two metrics", func(t *testing.T) {
		raw := []byte(`{"ts": ` + itoa(tsMS) + `, "vib_rms": 0.0123, "temp_c": 24.5}`)
		ms, reason, ok := parseSensorMessage("hsd/demo/printer-1/esp32-1", raw, testNow, testPast, testFuture)
		if !ok {
			t.Fatalf("dropped: %s", reason)
		}
		if len(ms) != 2 {
			t.Fatalf("got %d measurements, want 2", len(ms))
		}
		// Sorted by metric name.
		if ms[0].Metric != "temp_c" || ms[0].Value != 24.5 || ms[1].Metric != "vib_rms" || ms[1].Value != 0.0123 {
			t.Fatalf("unexpected metrics: %+v", ms)
		}
		for _, m := range ms {
			if m.SiteID != "demo" || m.MachineID != "printer-1" || m.SensorID != "esp32-1" {
				t.Fatalf("ids from topic wrong: %+v", m)
			}
			if m.TS != tsMS || m.ReceivedAt != nowMS {
				t.Fatalf("times wrong: ts=%d recv=%d", m.TS, m.ReceivedAt)
			}
		}
	})

	t.Run("missing ts -> arrival time", func(t *testing.T) {
		ms, _, ok := parseSensorMessage("hsd/s/m/x", []byte(`{"vib_rms":0.1}`), testNow, testPast, testFuture)
		if !ok || ms[0].TS != nowMS {
			t.Fatalf("ok=%v ms=%+v", ok, ms)
		}
	})

	t.Run("non-numeric fields ignored", func(t *testing.T) {
		raw := []byte(`{"vib_rms":0.1,"fw":"1.2.0","nested":{"a":1},"ok":true,"Bad Name":3}`)
		ms, _, ok := parseSensorMessage("hsd/s/m/x", raw, testNow, testPast, testFuture)
		if !ok || len(ms) != 1 || ms[0].Metric != "vib_rms" {
			t.Fatalf("ok=%v ms=%+v", ok, ms)
		}
	})

	t.Run("metric count is capped", func(t *testing.T) {
		raw := []byte(`{`)
		for i := 0; i < 40; i++ {
			if i > 0 {
				raw = append(raw, ',')
			}
			raw = append(raw, []byte(`"m`+itoa(int64(i))+`":1`)...)
		}
		raw = append(raw, '}')
		ms, _, ok := parseSensorMessage("hsd/s/m/x", raw, testNow, testPast, testFuture)
		if !ok || len(ms) != maxMetricsPerMessage {
			t.Fatalf("ok=%v len=%d", ok, len(ms))
		}
	})

	drops := []struct {
		name, topic, payload string
	}{
		{"bad json", "hsd/s/m/x", `}{`},
		{"json array", "hsd/s/m/x", `[1,2]`},
		{"json null", "hsd/s/m/x", `null`},
		{"only ts", "hsd/s/m/x", `{"ts":1}`},
		{"wrong prefix", "sensors/x/temperature", `{"vib_rms":1}`},
		{"too few segments", "hsd/s/m", `{"vib_rms":1}`},
		{"too many segments", "hsd/s/m/x/status", `{"vib_rms":1}`},
		{"uppercase id", "hsd/S/m/x", `{"vib_rms":1}`},
		{"empty id", "hsd//m/x", `{"vib_rms":1}`},
	}
	for _, c := range drops {
		t.Run("drop: "+c.name, func(t *testing.T) {
			if _, _, ok := parseSensorMessage(c.topic, []byte(c.payload), testNow, testPast, testFuture); ok {
				t.Fatalf("expected drop")
			}
		})
	}
}

func TestResolveEventTime(t *testing.T) {
	nowMS := testNow.UnixMilli()
	cases := []struct {
		name  string
		ts    int64
		hasTS bool
		want  int64
	}{
		{"no timestamp -> arrival", 0, false, nowMS},
		{"recent within window -> honored", nowMS - 2000, true, nowMS - 2000},
		{"48h ago within window -> honored", testNow.Add(-48 * time.Hour).UnixMilli(), true, testNow.Add(-48 * time.Hour).UnixMilli()},
		{"unsynced clock (1970) -> arrival", 5000, true, nowMS},
		{"seconds instead of ms -> arrival", testNow.Unix(), true, nowMS},
		{"far future -> arrival", testNow.Add(24 * time.Hour).UnixMilli(), true, nowMS},
		{"slight skew ahead -> honored", nowMS + 60_000, true, nowMS + 60_000},
		{"just past lower edge -> arrival", nowMS - testPast.Milliseconds() - 1, true, nowMS},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveEventTime(c.ts, c.hasTS, nowMS, testPast, testFuture); got != c.want {
				t.Errorf("resolveEventTime = %d, want %d", got, c.want)
			}
		})
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
