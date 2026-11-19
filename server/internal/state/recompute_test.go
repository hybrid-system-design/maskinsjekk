package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"maskinsjekk/internal/config"
	"maskinsjekk/internal/store"
)

func TestJobs(t *testing.T) {
	ev := func(s string, a, b int64) store.StateEvent { return store.StateEvent{State: s, Start: a, End: b} }
	got := Jobs([]store.StateEvent{
		ev("IDLE", 0, 10),
		ev("HEATING", 10, 20), ev("RUNNING", 20, 50), // job 1
		ev("IDLE", 50, 60),
		ev("RUNNING", 60, 70), ev("UNKNOWN", 70, 80), ev("RUNNING", 80, 90), // job 2 across a data gap
		ev("OFF", 90, 100),
		ev("RUNNING", 100, 110), ev("UNKNOWN", 110, 120), ev("IDLE", 120, 130), // job 3 ends at last busy
		ev("HEATING", 130, 0), // job 4, ongoing
	})
	want := []Job{{10, 50}, {60, 90}, {100, 110}, {130, 0}}
	if len(got) != len(want) {
		t.Fatalf("jobs = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("jobs = %v, want %v", got, want)
		}
	}
	if len(Jobs(nil)) != 0 {
		t.Fatal("jobs from no events")
	}
}

func TestConfigHash(t *testing.T) {
	a, ca, err := ConfigHash(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	b, _, _ := ConfigHash(testConfig())
	if a != b || ca == "" {
		t.Fatalf("same config, different hash: %s %s", a, b)
	}
	cfg := testConfig()
	cfg.Rules[1].When["power"] = config.Range{Below: f(25)}
	if c, _, _ := ConfigHash(cfg); c == a {
		t.Fatal("changed threshold, same hash")
	}
}

func TestSyncArchivesAndRecomputes(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	t0 := time.Date(2026, 11, 19, 13, 0, 0, 0, time.UTC)
	machines := config.Machines{Sites: []config.Site{{ID: "demo", Machines: []config.Machine{{ID: "m1", State: testConfig()}}}}}
	if err := st.SyncRegistry(ctx, machines); err != nil {
		t.Fatal(err)
	}
	// 10 min idle, then 10 min at 150 W with no vibration data.
	var ms []store.Measurement
	for ts := t0; ts.Before(t0.Add(20 * time.Minute)); ts = ts.Add(time.Second) {
		p := 6.0
		if !ts.Before(t0.Add(10 * time.Minute)) {
			p = 150
		}
		ms = append(ms, store.Measurement{SiteID: "demo", MachineID: "m1", SensorID: "plug", Metric: "power_w", Value: p, TS: ts.UnixMilli(), ReceivedAt: ts.UnixMilli()})
	}
	if err := st.InsertMeasurements(ctx, ms); err != nil {
		t.Fatal(err)
	}
	// Events recorded before rule sets existed: one long HEATING.
	if err := st.RecordState(ctx, "m1", "IDLE", t0.UnixMilli(), "old"); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordState(ctx, "m1", "HEATING", t0.Add(10*time.Minute).UnixMilli(), "old"); err != nil {
		t.Fatal(err)
	}
	logf := func(f string, a ...any) { t.Logf(f, a...) }
	now := t0.Add(20 * time.Minute)
	states := func(ruleSet int64) []string {
		evs, err := st.StateEvents(ctx, "m1", ruleSet, 0, now.Add(time.Hour).UnixMilli())
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, ev := range evs {
			out = append(out, ev.State)
		}
		return out
	}

	if err := Sync(ctx, st, machines.StateMachines(), now, logf); err != nil {
		t.Fatal(err)
	}
	sets, err := st.RuleSets(ctx, "m1")
	if err != nil || len(sets) != 2 {
		t.Fatalf("rule sets after first sync: %+v %v", sets, err)
	}
	cur, legacy := sets[0], sets[1]
	if cur.ArchivedAt != 0 || cur.Config == "" || legacy.ArchivedAt == 0 || legacy.Config != "" {
		t.Fatalf("rule sets: %+v", sets)
	}
	// testConfig: vib is optional, so 150 W without vibration is RUNNING.
	if got := states(0); len(got) != 2 || got[0] != "IDLE" || got[1] != "RUNNING" {
		t.Fatalf("recomputed timeline %v, want [IDLE RUNNING]", got)
	}
	if got := states(legacy.ID); len(got) != 2 || got[1] != "HEATING" {
		t.Fatalf("archived timeline %v", got)
	}
	if ev, ok, _ := st.CurrentState(ctx, "m1"); !ok || ev.State != "RUNNING" {
		t.Fatalf("current state after recompute: %+v %v", ev, ok)
	}

	// Same rules again: nothing changes.
	if err := Sync(ctx, st, machines.StateMachines(), now.Add(time.Minute), logf); err != nil {
		t.Fatal(err)
	}
	if sets, _ := st.RuleSets(ctx, "m1"); len(sets) != 2 {
		t.Fatalf("unchanged rules created a rule set: %+v", sets)
	}

	// New rules: the recomputed timeline is archived in turn.
	machines.Sites[0].Machines[0].State.Rules[1].When["power"] = config.Range{Below: f(200)}
	if err := Sync(ctx, st, machines.StateMachines(), now.Add(2*time.Minute), logf); err != nil {
		t.Fatal(err)
	}
	sets, _ = st.RuleSets(ctx, "m1")
	if len(sets) != 3 || sets[1].ArchivedAt == 0 {
		t.Fatalf("rule sets after change: %+v", sets)
	}
	if got := states(0); len(got) != 1 || got[0] != "IDLE" {
		t.Fatalf("timeline under new rules %v, want [IDLE]", got)
	}

	// Energy: 10 min at 150 W ≈ 25 Wh from the power rollups (no counter).
	wh, ok, err := st.EnergyWh(ctx, "m1", t0.Add(10*time.Minute).UnixMilli(), now.UnixMilli())
	if err != nil || !ok || wh < 24.9 || wh > 25.1 {
		t.Fatalf("energy = %v %v %v, want 25 Wh", wh, ok, err)
	}
}
