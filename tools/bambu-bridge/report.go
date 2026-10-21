package main

// Report handling for the Bambu Lab printer's local MQTT status feed. These
// are pure functions so they can be unit- and fuzz-tested without a printer.

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// stateCodes maps the printer's gcode_state to a number, because the
// maskinsjekk server stores numeric fields only. Unknown states map to -1.
var stateCodes = map[string]float64{
	"IDLE":    0,
	"PREPARE": 1,
	"RUNNING": 2,
	"PAUSE":   3,
	"FINISH":  4,
	"FAILED":  5,
	"SLICING": 6,
}

// activeStates are the gcode_states in which a job is in progress.
var activeStates = map[string]bool{"PREPARE": true, "RUNNING": true, "PAUSE": true, "SLICING": true}

// doorBit is the door switch in the hex "stat" field on enclosed printers
// (confirmed on a P2S: 248000 closed, A48000 open).
const doorBit = 0x00800000

// numericFields maps report fields to the metric names published.
var numericFields = []struct{ report, metric string }{
	{"mc_percent", "percent"},
	{"mc_remaining_time", "remaining_min"},
	{"layer_num", "layer"},
	{"total_layer_num", "total_layers"},
	{"stg_cur", "stage"}, // print sub-stage (heating, leveling, ...); see README
	{"print_error", "error_code"},
	{"nozzle_temper", "nozzle_c"},
	{"nozzle_target_temper", "nozzle_target_c"},
	{"bed_temper", "bed_c"},
	{"bed_target_temper", "bed_target_c"},
	{"chamber_temper", "chamber_c"},
}

// mergeReport merges the "print" object of one device/<serial>/report payload
// into state. Reports after the first are often partial (changed fields only),
// so state accumulates. It returns false for payloads without a "print"
// object (other report kinds, bad JSON); state is then unchanged.
func mergeReport(state map[string]any, payload []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.UseNumber()
	var msg map[string]any
	if err := dec.Decode(&msg); err != nil {
		return false
	}
	report, ok := msg["print"].(map[string]any)
	if !ok {
		return false
	}
	for k, v := range report {
		state[k] = v
	}
	return true
}

// gcodeState returns the upper-cased gcode_state, or "" if unknown.
func gcodeState(state map[string]any) string {
	s, _ := state["gcode_state"].(string)
	return strings.ToUpper(strings.TrimSpace(s))
}

// toFloat accepts JSON numbers and numeric strings; NaN/Inf are rejected.
func toFloat(v any) (float64, bool) {
	var f float64
	switch x := v.(type) {
	case json.Number:
		var err error
		if f, err = x.Float64(); err != nil {
			return 0, false
		}
	case float64:
		f = x
	case string:
		var err error
		if f, err = strconv.ParseFloat(strings.TrimSpace(x), 64); err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// metrics converts the merged printer state into numeric fields. Fields the
// printer hasn't reported are left out rather than guessed.
func metrics(state map[string]any) map[string]float64 {
	m := map[string]float64{}
	if gs := gcodeState(state); gs != "" {
		code, known := stateCodes[gs]
		if !known {
			code = -1
		}
		m["state_code"] = code
		m["active"] = 0
		if activeStates[gs] {
			m["active"] = 1
		}
	}
	for _, f := range numericFields {
		if v, ok := toFloat(state[f.report]); ok {
			m[f.metric] = v
		}
	}
	if s, ok := state["stat"].(string); ok {
		if v, err := strconv.ParseUint(strings.TrimSpace(s), 16, 64); err == nil {
			m["door_open"] = 0
			if v&doorBit != 0 {
				m["door_open"] = 1
			}
		}
	}
	return m
}

// buildPayload renders a maskinsjekk sensor message. While the printer is
// unreachable only printer_reachable=0 is sent, never stale values.
func buildPayload(state map[string]any, reachable bool, tsMS int64) []byte {
	out := map[string]any{"ts": tsMS}
	if reachable {
		for k, v := range metrics(state) {
			out[k] = v
		}
		out["printer_reachable"] = 1
	} else {
		out["printer_reachable"] = 0
	}
	b, _ := json.Marshal(out) // only numbers: cannot fail
	return b
}

// changeKey identifies transitions worth publishing immediately instead of
// waiting for the next interval tick.
func changeKey(state map[string]any) string {
	m := metrics(state)
	return strconv.FormatFloat(m["state_code"], 'f', -1, 64) + "/" +
		strconv.FormatFloat(m["stage"], 'f', -1, 64) + "/" +
		strconv.FormatFloat(m["door_open"], 'f', -1, 64)
}
