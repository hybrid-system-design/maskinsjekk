package config

import (
	"encoding/json"
	"fmt"
	"time"
)

// Machine states. A machine's state rules may use any subset of the
// rule-based ones; Unknown is never configured, it is what the state machine
// reports when a required signal has no fresh data.
const (
	StateOff     = "OFF"
	StateIdle    = "IDLE"
	StateHeating = "HEATING"
	StateRunning = "RUNNING"
	StateUnknown = "UNKNOWN"
)

// ruleStates are the states a rule in the machines file may produce.
var ruleStates = map[string]bool{StateOff: true, StateIdle: true, StateHeating: true, StateRunning: true}

// StateConfig is a machine's state detection: named input signals and an
// ordered list of rules. The first rule whose conditions all hold gives the
// candidate state; a rule without conditions always holds (use it last as the
// default). The candidate must hold for the rule's enter_after before the
// machine switches to it.
type StateConfig struct {
	Signals map[string]Signal `json:"signals"`
	Rules   []StateRule       `json:"rules"`
}

// Signal is one input of the state rules: a metric (optionally from one
// sensor only), averaged over Window. When no sample falls in the window the
// latest sample is used if it is at most MaxAge old (sparse sources such as
// power meters only report on change); otherwise the signal is missing.
type Signal struct {
	Sensor string   `json:"sensor,omitempty"`
	Metric string   `json:"metric"`
	Window Duration `json:"window"`
	MaxAge Duration `json:"max_age,omitempty"` // default: Window
	// Optional signals may be missing without the state becoming UNKNOWN;
	// rules that test a missing signal just don't match.
	Optional bool `json:"optional,omitempty"`
}

// StateRule maps conditions on signals to a state.
type StateRule struct {
	State      string           `json:"state"`
	When       map[string]Range `json:"when,omitempty"`
	EnterAfter Duration         `json:"enter_after,omitempty"`
}

// Range is a condition on one signal: AtLeast <= value < Below. Either bound
// may be omitted.
type Range struct {
	AtLeast *float64 `json:"at_least,omitempty"`
	Below   *float64 `json:"below,omitempty"`
}

// Contains reports whether v satisfies the range.
func (r Range) Contains(v float64) bool {
	if r.AtLeast != nil && v < *r.AtLeast {
		return false
	}
	if r.Below != nil && v >= *r.Below {
		return false
	}
	return true
}

// Duration is a time.Duration written as a Go duration string ("90s", "5m")
// in JSON.
type Duration time.Duration

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

// UnmarshalJSON parses a Go duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string like \"60s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalJSON writes a Go duration string.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.D().String()) }

// maxSignalAge bounds window and max_age so a typo can't make the evaluator
// scan days of raw data on every tick.
const maxSignalAge = 24 * time.Hour

// validate checks a state config and fills in defaults (MaxAge).
func (c *StateConfig) validate() error {
	if len(c.Signals) == 0 {
		return fmt.Errorf("state: no signals")
	}
	required := 0
	for name, sig := range c.Signals {
		if !ValidID(name) {
			return fmt.Errorf("state: invalid signal name %q", name)
		}
		if !ValidID(sig.Metric) {
			return fmt.Errorf("state: signal %s: invalid metric %q", name, sig.Metric)
		}
		if sig.Sensor != "" && !ValidID(sig.Sensor) {
			return fmt.Errorf("state: signal %s: invalid sensor %q", name, sig.Sensor)
		}
		if sig.Window <= 0 || sig.Window.D() > maxSignalAge {
			return fmt.Errorf("state: signal %s: window must be between 0 and %s", name, maxSignalAge)
		}
		if sig.MaxAge == 0 {
			sig.MaxAge = sig.Window
		}
		if sig.MaxAge < sig.Window || sig.MaxAge.D() > maxSignalAge {
			return fmt.Errorf("state: signal %s: max_age must be between window and %s", name, maxSignalAge)
		}
		if !sig.Optional {
			required++
		}
		c.Signals[name] = sig
	}
	if required == 0 {
		return fmt.Errorf("state: at least one signal must be required (not optional)")
	}
	if len(c.Rules) == 0 {
		return fmt.Errorf("state: no rules")
	}
	for i, r := range c.Rules {
		if !ruleStates[r.State] {
			return fmt.Errorf("state: rule %d: unknown state %q (use OFF, IDLE, HEATING or RUNNING)", i+1, r.State)
		}
		if r.EnterAfter < 0 || r.EnterAfter.D() > maxSignalAge {
			return fmt.Errorf("state: rule %d: enter_after out of range", i+1)
		}
		for sig, rg := range r.When {
			if _, ok := c.Signals[sig]; !ok {
				return fmt.Errorf("state: rule %d: unknown signal %q", i+1, sig)
			}
			if rg.AtLeast == nil && rg.Below == nil {
				return fmt.Errorf("state: rule %d: condition on %s needs at_least and/or below", i+1, sig)
			}
		}
	}
	return nil
}
