package state

import (
	"context"
	"fmt"
	"log"
	"time"

	"maskinsjekk/internal/config"
	"maskinsjekk/internal/store"
)

// Runner evaluates the state of every machine with state rules on a fixed
// interval and records transitions as state events.
type Runner struct {
	store    *store.Store
	machines []*machine
	interval time.Duration
	log      *log.Logger
	now      func() time.Time
}

type machine struct {
	id      string
	cfg     *config.StateConfig
	tracker *Tracker
	resumed bool
}

// NewRunner builds a runner for the machines that have a state config.
func NewRunner(st *store.Store, machines []config.Machine, interval time.Duration, logger *log.Logger) *Runner {
	r := &Runner{store: st, interval: interval, log: logger, now: time.Now}
	for _, mc := range machines {
		if mc.State != nil {
			r.machines = append(r.machines, &machine{id: mc.ID, cfg: mc.State, tracker: NewTracker(mc.State)})
		}
	}
	return r
}

// Run evaluates every interval until ctx is done.
func (r *Runner) Run(ctx context.Context) {
	if len(r.machines) == 0 {
		return
	}
	r.log.Printf("state: evaluating %d machine(s) every %s", len(r.machines), r.interval)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		r.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Tick evaluates every machine once.
func (r *Runner) Tick(ctx context.Context) {
	nowMS := r.now().UnixMilli()
	for _, m := range r.machines {
		if err := r.evaluate(ctx, m, nowMS); err != nil {
			r.log.Printf("state: %s: %v", m.id, err)
		}
	}
}

func (r *Runner) evaluate(ctx context.Context, m *machine, nowMS int64) error {
	// Continue from the open state event (after a restart) rather than
	// opening a duplicate; retried every tick until the read succeeds.
	if !m.resumed {
		ev, ok, err := r.store.CurrentState(ctx, m.id)
		if err != nil {
			return fmt.Errorf("resume: %w", err)
		}
		if ok {
			m.tracker.Resume(ev.State, ev.Start)
		}
		m.resumed = true
	}
	vals := Values{}
	for name, sig := range m.cfg.Signals {
		since := nowMS - max(sig.Window, sig.MaxAge).D().Milliseconds()
		rows, err := r.store.RecentSamples(ctx, m.id, sig.Sensor, sig.Metric, since)
		if err != nil {
			return err
		}
		samples := make([]Sample, len(rows))
		for i, row := range rows {
			samples[i] = Sample{TS: row.TS, Value: row.Value}
		}
		if v, ok := SignalValue(samples, nowMS, sig); ok {
			vals[name] = v
		}
	}
	cand, reason := Classify(m.cfg, vals)
	before := *m.tracker
	tr, changed := m.tracker.Step(nowMS, cand, reason)
	if !changed {
		return nil
	}
	if err := r.store.RecordState(ctx, m.id, tr.State, tr.Start, tr.Reason); err != nil {
		// Forget the transition so the next tick retries it.
		*m.tracker = before
		return err
	}
	r.log.Printf("state: %s -> %s (%s)", m.id, tr.State, tr.Reason)
	return nil
}
