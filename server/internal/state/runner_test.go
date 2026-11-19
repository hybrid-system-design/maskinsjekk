package state

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"maskinsjekk/internal/config"
	"maskinsjekk/internal/store"
)

func TestRunner(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	cfg := testConfig()
	machines := config.Machines{Sites: []config.Site{{ID: "demo", Machines: []config.Machine{
		{ID: "m1", State: cfg},
		{ID: "plain"}, // no state config: ignored
	}}}}
	if err := st.SyncRegistry(ctx, machines); err != nil {
		t.Fatal(err)
	}

	t0 := time.Date(2026, 11, 13, 13, 0, 0, 0, time.UTC)
	clock := t0
	newRunner := func() *Runner {
		r := NewRunner(st, machines.StateMachines(), time.Second, log.New(io.Discard, "", 0))
		r.now = func() time.Time { return clock }
		return r
	}
	// emit writes one sample per second of power (and vibration) for d.
	emit := func(from time.Time, d time.Duration, power, vib float64) {
		var ms []store.Measurement
		for ts := from; ts.Before(from.Add(d)); ts = ts.Add(time.Second) {
			ms = append(ms,
				store.Measurement{SiteID: "demo", MachineID: "m1", SensorID: "plug", Metric: "power_w", Value: power, TS: ts.UnixMilli(), ReceivedAt: ts.UnixMilli()},
				store.Measurement{SiteID: "demo", MachineID: "m1", SensorID: "esp", Metric: "vib_rms", Value: vib, TS: ts.UnixMilli(), ReceivedAt: ts.UnixMilli()})
		}
		if err := st.InsertMeasurements(ctx, ms); err != nil {
			t.Fatal(err)
		}
	}
	// runUntil ticks every 5 s from the current clock up to end.
	runUntil := func(r *Runner, end time.Time) {
		for ; !clock.After(end); clock = clock.Add(5 * time.Second) {
			r.Tick(ctx)
		}
	}
	current := func() store.StateEvent {
		ev, ok, err := st.CurrentState(ctx, "m1")
		if err != nil || !ok {
			t.Fatalf("current state: ok=%v err=%v", ok, err)
		}
		return ev
	}

	r := newRunner()
	if len(r.machines) != 1 {
		t.Fatalf("runner machines = %d, want 1", len(r.machines))
	}

	// No data at all: UNKNOWN.
	r.Tick(ctx)
	if ev := current(); ev.State != config.StateUnknown {
		t.Fatalf("no data: %+v", ev)
	}

	// 10 min idle, then a job: 3 min heating, 10 min running, back to idle.
	emit(t0, 10*time.Minute, 6, 0.01)
	emit(t0.Add(10*time.Minute), 3*time.Minute, 300, 0.01)
	emit(t0.Add(13*time.Minute), 10*time.Minute, 150, 0.04)
	emit(t0.Add(23*time.Minute), 10*time.Minute, 10, 0.03)
	runUntil(r, t0.Add(20*time.Minute))
	if ev := current(); ev.State != config.StateRunning {
		t.Fatalf("mid-job: %+v", ev)
	}

	// Restart mid-job: the new runner resumes RUNNING instead of re-opening it.
	r = newRunner()
	ctxRun, cancel := context.WithCancel(ctx)
	cancel()
	r.Run(ctxRun) // resumes, ticks once, returns
	runUntil(r, t0.Add(33*time.Minute))
	if ev := current(); ev.State != config.StateIdle {
		t.Fatalf("after job: %+v", ev)
	}

	// Data stops: once power is older than max_age (10 min) the state is UNKNOWN.
	runUntil(r, t0.Add(44*time.Minute))
	if ev := current(); ev.State != config.StateUnknown {
		t.Fatalf("after data stopped: %+v", ev)
	}

	evs, err := st.StateEvents(ctx, "m1", 0, t0.Add(-time.Hour).UnixMilli(), clock.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for i, ev := range evs {
		got = append(got, ev.State)
		t.Logf("%s %-8s %s", time.UnixMilli(ev.Start).UTC().Format("15:04:05"), ev.State, ev.Reason)
		if i > 0 && evs[i-1].End != ev.Start {
			t.Errorf("event %d: previous ends %d, this starts %d", i, evs[i-1].End, ev.Start)
		}
	}
	want := []string{"UNKNOWN", "IDLE", "HEATING", "RUNNING", "IDLE", "UNKNOWN"}
	if len(got) != len(want) {
		t.Fatalf("events %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events %v, want %v", got, want)
		}
	}
	// Transitions are backdated to when the data changed, within one window.
	if d := time.UnixMilli(evs[2].Start).Sub(t0.Add(10 * time.Minute)); d < 0 || d > time.Minute {
		t.Errorf("HEATING starts %s after the power rise", d)
	}
	if evs[len(evs)-1].End != 0 {
		t.Errorf("current event is closed")
	}

	// A range inside one event returns just that event.
	mid := t0.Add(18 * time.Minute).UnixMilli()
	evs, err = st.StateEvents(ctx, "m1", 0, mid, mid+1)
	if err != nil || len(evs) != 1 || evs[0].State != config.StateRunning {
		t.Fatalf("narrow range: %+v %v", evs, err)
	}
}
