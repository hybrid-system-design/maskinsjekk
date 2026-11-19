package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"maskinsjekk/internal/config"
	"maskinsjekk/internal/store"
)

// replayStep is the evaluation step when recomputing history. Older history
// only has per-minute rollups, which this resolves without loss.
const replayStep = 10 * time.Second

// ConfigHash identifies a state config: equal rules give equal hashes, however
// the machines file is formatted. It also returns the canonical JSON.
func ConfigHash(cfg *config.StateConfig) (hash, canonical string, err error) {
	b, err := json.Marshal(cfg) // map keys sorted, durations as strings
	if err != nil {
		return "", "", fmt.Errorf("state config: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8]), string(b), nil
}

// Sync makes each machine's configured rules its current rule set. When the
// rules changed, the previously detected timeline is archived (still served
// by the API) and the machine's whole stored history (per-minute rollups are
// kept forever, raw data where it still exists) is replayed under the new
// rules, so the timeline and jobs always show what the current rules detect.
// Run it before the Runner starts.
func Sync(ctx context.Context, st *store.Store, machines []config.Machine, now time.Time, logf func(string, ...any)) error {
	for _, mc := range machines {
		if mc.State == nil {
			continue
		}
		hash, canonical, err := ConfigHash(mc.State)
		if err != nil {
			return err
		}
		setID, created, err := st.ActivateRuleSet(ctx, mc.ID, hash, canonical, now.UnixMilli())
		if err != nil {
			return fmt.Errorf("%s: %w", mc.ID, err)
		}
		if !created {
			continue // rules unchanged
		}
		from, ok, err := historyStart(ctx, st, mc)
		if err != nil {
			return fmt.Errorf("%s: %w", mc.ID, err)
		}
		if !ok {
			continue // no data yet: the Runner starts the timeline
		}
		started := time.Now()
		trs, err := replayStored(ctx, st, mc, from, now.UnixMilli())
		if err != nil {
			return fmt.Errorf("%s: recompute: %w", mc.ID, err)
		}
		evs := make([]store.StateEvent, len(trs))
		for i, tr := range trs {
			evs[i] = store.StateEvent{State: tr.State, Start: tr.Start, Reason: tr.Reason}
		}
		if err := st.ReplaceStateEvents(ctx, mc.ID, setID, evs); err != nil {
			return fmt.Errorf("%s: %w", mc.ID, err)
		}
		logf("state: %s: rules changed; archived the old timeline and recomputed %d transitions since %s in %s",
			mc.ID, len(evs), time.UnixMilli(from).UTC().Format(time.RFC3339), time.Since(started).Round(time.Millisecond))
	}
	return nil
}

// historyStart is when the machine's required signals first have data: the
// earliest point from which the rules can say anything.
func historyStart(ctx context.Context, st *store.Store, mc config.Machine) (int64, bool, error) {
	var from int64
	found := false
	for _, sig := range mc.State.Signals {
		if sig.Optional {
			continue
		}
		ts, ok, err := st.FirstSample(ctx, mc.ID, sig.Sensor, sig.Metric)
		if err != nil {
			return 0, false, err
		}
		if ok && (!found || ts < from) {
			from, found = ts, true
		}
	}
	return from, found, nil
}

// replayStored replays a machine's stored signals over [fromMS, toMS).
func replayStored(ctx context.Context, st *store.Store, mc config.Machine, fromMS, toMS int64) ([]Transition, error) {
	series := map[string][]Sample{}
	for name, sig := range mc.State.Signals {
		lookback := max(sig.Window, sig.MaxAge).D().Milliseconds()
		rows, err := st.SignalHistory(ctx, mc.ID, sig.Sensor, sig.Metric, fromMS-lookback)
		if err != nil {
			return nil, err
		}
		samples := make([]Sample, len(rows))
		for i, r := range rows {
			samples[i] = Sample{TS: r.TS, Value: r.Value}
		}
		series[name] = samples
	}
	return Replay(mc.State, series, fromMS, toMS, replayStep.Milliseconds()), nil
}
