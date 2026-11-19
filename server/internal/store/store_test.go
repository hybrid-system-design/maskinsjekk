package store

import (
	"context"
	"math"
	"path/filepath"
	"testing"

	"maskinsjekk/internal/config"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func meas(machine, metric string, v float64, ts int64) Measurement {
	return Measurement{SiteID: "demo", MachineID: machine, SensorID: "esp32-1", Metric: metric, Value: v, TS: ts, ReceivedAt: ts}
}

const t0 = int64(1_792_411_200_000) // 2026-10-19T12:00:00Z, minute-aligned

func TestMigrationsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "m.db")
	for i := 0; i < 2; i++ {
		st, err := Open(path)
		if err != nil {
			t.Fatalf("Open #%d: %v", i, err)
		}
		st.Close()
	}
}

func TestInsertAutoRegistersAndTracksLiveness(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	if err := st.InsertMeasurements(ctx, []Measurement{meas("printer-1", "vib_rms", 0.1, t0)}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	ms, err := st.Machines(ctx)
	if err != nil {
		t.Fatalf("machines: %v", err)
	}
	if len(ms) != 1 || ms[0].ID != "printer-1" || ms[0].SiteID != "demo" || ms[0].Name != "printer-1" {
		t.Fatalf("unexpected machines: %+v", ms)
	}
	if ms[0].LastSeenMS != t0 || len(ms[0].Sensors) != 1 || ms[0].Sensors[0].LastSeenMS != t0 {
		t.Fatalf("liveness not tracked: %+v", ms[0])
	}

	// An older (backfilled) arrival must not move last_seen backwards.
	if err := st.InsertMeasurements(ctx, []Measurement{meas("printer-1", "vib_rms", 0.1, t0-10_000)}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	ms, _ = st.Machines(ctx)
	if ms[0].LastSeenMS != t0 {
		t.Fatalf("last_seen moved backwards: %d", ms[0].LastSeenMS)
	}
}

func TestSyncRegistryNamesWin(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	_ = st.InsertMeasurements(ctx, []Measurement{meas("printer-1", "vib_rms", 0.1, t0)})

	cfg := config.Machines{Sites: []config.Site{{
		ID: "demo", Name: "HSD Demo",
		Machines: []config.Machine{{ID: "printer-1", Name: "3D-printer", Sensors: []config.Sensor{{ID: "esp32-1", Kind: "esp32-vibration"}}}},
	}}}
	for i := 0; i < 2; i++ { // idempotent
		if err := st.SyncRegistry(ctx, cfg); err != nil {
			t.Fatalf("sync: %v", err)
		}
	}
	ms, _ := st.Machines(ctx)
	if ms[0].Name != "3D-printer" || ms[0].Sensors[0].Kind != "esp32-vibration" {
		t.Fatalf("configured names not applied: %+v", ms[0])
	}
	if ms[0].LastSeenMS != t0 {
		t.Fatalf("sync clobbered liveness: %+v", ms[0])
	}
}

func TestRollupAndSeries(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	// Two minutes of 1 Hz data: minute 0 values 1..60, minute 1 constant 10.
	var batch []Measurement
	for i := 0; i < 60; i++ {
		batch = append(batch, meas("m1", "vib_rms", float64(i+1), t0+int64(i)*1000))
		batch = append(batch, meas("m1", "vib_rms", 10, t0+60_000+int64(i)*1000))
	}
	if err := st.InsertMeasurements(ctx, batch); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Minute buckets from rollups.
	got, err := st.Series(ctx, "m1", "vib_rms", t0, t0+120_000, 60_000)
	if err != nil {
		t.Fatalf("series: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d buckets, want 2: %+v", len(got), got)
	}
	if got[0].T != t0 || got[0].N != 60 || got[0].Min != 1 || got[0].Max != 60 || math.Abs(got[0].Avg-30.5) > 1e-9 {
		t.Fatalf("minute 0 wrong: %+v", got[0])
	}
	if got[1].Avg != 10 || got[1].N != 60 {
		t.Fatalf("minute 1 wrong: %+v", got[1])
	}

	// One 2-minute bucket re-aggregates rollups with a weighted mean.
	got, _ = st.Series(ctx, "m1", "vib_rms", t0, t0+120_000, 120_000)
	if len(got) != 1 || got[0].N != 120 || math.Abs(got[0].Avg-(30.5+10)/2) > 1e-9 {
		t.Fatalf("2-minute bucket wrong: %+v", got)
	}

	// Sub-minute buckets come from raw rows.
	got, _ = st.Series(ctx, "m1", "vib_rms", t0, t0+10_000, 5_000)
	if len(got) != 2 || got[0].N != 5 || got[0].Avg != 3 {
		t.Fatalf("raw buckets wrong: %+v", got)
	}

	// Other machines/metrics don't leak in.
	got, _ = st.Series(ctx, "m1", "temp_c", t0, t0+120_000, 60_000)
	if len(got) != 0 {
		t.Fatalf("unexpected temp_c data: %+v", got)
	}
}

func TestPruneKeepsRollups(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	_ = st.InsertMeasurements(ctx, []Measurement{
		meas("m1", "vib_rms", 1, t0),
		meas("m1", "vib_rms", 3, t0+1000),
	})
	n, err := st.PruneRaw(ctx, t0+60_000)
	if err != nil || n != 2 {
		t.Fatalf("prune: n=%d err=%v", n, err)
	}
	raw, _ := st.Series(ctx, "m1", "vib_rms", t0, t0+60_000, 1_000)
	if len(raw) != 0 {
		t.Fatalf("raw rows survived prune: %+v", raw)
	}
	roll, _ := st.Series(ctx, "m1", "vib_rms", t0, t0+60_000, 60_000)
	if len(roll) != 1 || roll[0].Avg != 2 || roll[0].N != 2 {
		t.Fatalf("rollup lost after prune: %+v", roll)
	}
	metrics, _ := st.Metrics(ctx, "m1")
	if len(metrics) != 1 || metrics[0] != "vib_rms" {
		t.Fatalf("metrics: %v", metrics)
	}
}

func TestFloorMod(t *testing.T) {
	if floorMod(-1, 60_000) != 59_999 || floorMod(61_000, 60_000) != 1000 {
		t.Fatal("floorMod wrong")
	}
}

func TestSignalHistoryAndEnergy(t *testing.T) {
	st := openTest(t)
	ctx := context.Background()
	const minute = int64(60_000)
	base := int64(1_000_000_000) // not minute-aligned on purpose
	var ms []Measurement
	// 10 minutes, one sample every 10 s: counter rising 1 Wh per minute.
	for ts := int64(0); ts < 10*minute; ts += 10_000 {
		ms = append(ms, meas("m", "energy_wh", 100+float64(ts)/float64(minute), base+ts))
	}
	if err := st.InsertMeasurements(ctx, ms); err != nil {
		t.Fatal(err)
	}
	wh, ok, err := st.EnergyWh(ctx, "m", base+2*minute, base+7*minute)
	if err != nil || !ok || wh < 4.8 || wh > 5 {
		t.Fatalf("energy from raw counter = %v %v %v, want ~5", wh, ok, err)
	}
	all, err := st.SignalHistory(ctx, "m", "", "energy_wh", base)
	if err != nil || len(all) != 60 {
		t.Fatalf("raw history: %d samples, err %v", len(all), err)
	}

	// Prune the first 5 minutes of raw data: history fills in from rollups.
	if _, err := st.db.Exec(`DELETE FROM measurements WHERE ts < ?`, base+5*minute); err != nil {
		t.Fatal(err)
	}
	h, err := st.SignalHistory(ctx, "m", "", "energy_wh", base)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 5+30 || h[0].TS != base-base%minute+minute/2 || h[5].TS != base+5*minute {
		t.Fatalf("mixed history: %d samples, first %+v, raw start %+v", len(h), h[0], h[5])
	}
	for i := 1; i < len(h); i++ {
		if h[i].TS <= h[i-1].TS {
			t.Fatalf("history not sorted at %d", i)
		}
	}
	if wh, ok, _ := st.EnergyWh(ctx, "m", base, base+3*minute); !ok || wh <= 0 {
		t.Fatalf("energy from counter rollups = %v %v", wh, ok)
	}
	if _, ok, _ := st.EnergyWh(ctx, "nope", base, base+minute); ok {
		t.Fatal("energy for a machine without data")
	}
}
