// Package state infers a machine's operating state (OFF, IDLE, HEATING,
// RUNNING) from its sensor signals, using the per-machine rules in the
// machines file. Nothing here knows what kind of machine it is.
//
// The core is pure and clock-free so the same code runs live (Runner, on a
// ticker against the database) and offline (Replay, over stored history):
//
//	samples --SignalValue--> values --Classify--> candidate --Tracker--> transitions
package state

import (
	"fmt"
	"sort"
	"strings"

	"maskinsjekk/internal/config"
)

// Sample is one stored value of a signal's metric.
type Sample struct {
	TS    int64 // unix ms
	Value float64
}

// SignalValue reduces a signal's samples to one value at nowMS: the mean of
// the samples in (now-window, now], or else the latest sample at most maxAge
// old. Samples must be sorted by TS; those after nowMS are ignored. ok is false
// when the signal has no fresh data.
func SignalValue(samples []Sample, nowMS int64, sig config.Signal) (v float64, ok bool) {
	// Index of the first sample after now.
	end := sort.Search(len(samples), func(i int) bool { return samples[i].TS > nowMS })
	if end == 0 {
		return 0, false
	}
	winStart := nowMS - sig.Window.D().Milliseconds()
	var sum float64
	n := 0
	for i := end - 1; i >= 0 && samples[i].TS > winStart; i-- {
		sum += samples[i].Value
		n++
	}
	if n > 0 {
		return sum / float64(n), true
	}
	last := samples[end-1]
	if nowMS-last.TS <= sig.MaxAge.D().Milliseconds() {
		return last.Value, true
	}
	return 0, false
}

// Values maps signal name to its current value; a missing key is a signal
// without fresh data.
type Values map[string]float64

// Classify returns the candidate state for the current signal values, and a
// short human-readable reason (the values it was based on). A missing
// required signal gives UNKNOWN; a rule testing a missing optional signal
// does not match. No matching rule also gives UNKNOWN.
func Classify(cfg *config.StateConfig, vals Values) (string, string) {
	reason := formatValues(cfg, vals)
	var missing []string
	for name, sig := range cfg.Signals {
		if _, ok := vals[name]; !ok && !sig.Optional {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return config.StateUnknown, "no data: " + strings.Join(missing, ", ")
	}
	for _, r := range cfg.Rules {
		if matches(r, vals) {
			return r.State, reason
		}
	}
	return config.StateUnknown, "no rule matched: " + reason
}

func matches(r config.StateRule, vals Values) bool {
	for name, rg := range r.When {
		v, ok := vals[name]
		if !ok || !rg.Contains(v) {
			return false
		}
	}
	return true
}

func formatValues(cfg *config.StateConfig, vals Values) string {
	names := make([]string, 0, len(cfg.Signals))
	for name := range cfg.Signals {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		if v, ok := vals[name]; ok {
			parts = append(parts, fmt.Sprintf("%s=%.4g", name, v))
		} else {
			parts = append(parts, name+"=-")
		}
	}
	return strings.Join(parts, " ")
}

// Transition is a change of state. Start is when the new state's candidate
// first appeared, not when the debounce confirmed it, so timelines line up
// with the data.
type Transition struct {
	State  string
	Start  int64 // unix ms
	Reason string
}

// Tracker debounces candidates into confirmed states: a candidate must hold
// continuously for its rule's enter_after before the state changes.
type Tracker struct {
	cfg        *config.StateConfig
	current    string
	since      int64
	cand       string
	candSince  int64
	candReason string
}

// NewTracker returns a tracker with no state; the first Step adopts the
// candidate immediately.
func NewTracker(cfg *config.StateConfig) *Tracker { return &Tracker{cfg: cfg} }

// Resume sets the tracker's state, e.g. the open state event after a restart.
func (t *Tracker) Resume(state string, since int64) {
	t.current, t.since = state, since
	t.cand, t.candSince = state, since
}

// Current returns the confirmed state and when it started ("" if none yet).
func (t *Tracker) Current() (string, int64) { return t.current, t.since }

// Step feeds the candidate at nowMS and reports a transition when the state
// changes.
func (t *Tracker) Step(nowMS int64, cand, reason string) (Transition, bool) {
	if t.current == "" {
		t.Resume(cand, nowMS)
		return Transition{State: cand, Start: nowMS, Reason: reason}, true
	}
	if cand == t.current {
		t.cand, t.candSince = cand, nowMS
		return Transition{}, false
	}
	if cand != t.cand {
		t.cand, t.candSince, t.candReason = cand, nowMS, reason
	}
	if nowMS-t.candSince < t.enterAfter(cand) {
		return Transition{}, false
	}
	// Never start before the current state did (clock oddities on resume).
	start := max(t.candSince, t.since)
	tr := Transition{State: cand, Start: start, Reason: t.candReason}
	t.current, t.since = cand, start
	return tr, true
}

// enterAfter is the debounce for entering a state: the longest enter_after of
// the rules producing it (several rules may share a state). UNKNOWN is entered
// at once: the signals' max_age already delayed it.
func (t *Tracker) enterAfter(state string) int64 {
	var d int64
	for _, r := range t.cfg.Rules {
		if r.State == state {
			d = max(d, r.EnterAfter.D().Milliseconds())
		}
	}
	return d
}

// Replay runs the state machine over stored history from fromMS to toMS in
// steps of stepMS and returns the transitions. series maps signal name to its
// samples, sorted by TS.
func Replay(cfg *config.StateConfig, series map[string][]Sample, fromMS, toMS, stepMS int64) []Transition {
	tr := NewTracker(cfg)
	var out []Transition
	for now := fromMS; now < toMS; now += stepMS {
		cand, reason := Classify(cfg, Evaluate(cfg, series, now))
		if t, ok := tr.Step(now, cand, reason); ok {
			out = append(out, t)
		}
	}
	return out
}

// Evaluate computes all signal values at nowMS from their samples.
func Evaluate(cfg *config.StateConfig, series map[string][]Sample, nowMS int64) Values {
	vals := Values{}
	for name, sig := range cfg.Signals {
		if v, ok := SignalValue(series[name], nowMS, sig); ok {
			vals[name] = v
		}
	}
	return vals
}
