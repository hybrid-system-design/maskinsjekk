package main

import (
	"encoding/json"
	"testing"
)

func decode(t *testing.T, b []byte) map[string]float64 {
	t.Helper()
	var m map[string]float64
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("payload %s is not a flat numeric object: %v", b, err)
	}
	return m
}

func TestMergeReportAccumulatesPartialReports(t *testing.T) {
	state := map[string]any{}
	full := `{"print":{"gcode_state":"RUNNING","mc_percent":10,"layer_num":5,"nozzle_temper":219.5,"stat":"248000","stg_cur":0}}`
	if !mergeReport(state, []byte(full)) {
		t.Fatal("full report rejected")
	}
	if !mergeReport(state, []byte(`{"print":{"mc_percent":"11","command":"push_status"}}`)) {
		t.Fatal("partial report rejected")
	}
	m := metrics(state)
	want := map[string]float64{
		"state_code": 2, "active": 1, "percent": 11, "layer": 5,
		"nozzle_c": 219.5, "door_open": 0, "stage": 0,
	}
	for k, v := range want {
		if m[k] != v {
			t.Errorf("%s = %v, want %v", k, m[k], v)
		}
	}
	if _, ok := m["bed_c"]; ok {
		t.Error("bed_c published although never reported")
	}
}

func TestMergeReportIgnoresOtherMessages(t *testing.T) {
	state := map[string]any{"gcode_state": "IDLE"}
	for _, p := range []string{``, `nope`, `[]`, `{"info":{"command":"get_version"}}`, `{"print":"x"}`} {
		if mergeReport(state, []byte(p)) {
			t.Errorf("accepted %q", p)
		}
	}
	if len(state) != 1 {
		t.Errorf("state changed: %v", state)
	}
}

func TestMetricsStatesAndDoor(t *testing.T) {
	cases := []struct {
		state        map[string]any
		code, active float64
		door         float64
		hasDoor      bool
	}{
		{map[string]any{"gcode_state": "idle"}, 0, 0, 0, false},
		{map[string]any{"gcode_state": "PREPARE"}, 1, 1, 0, false},
		{map[string]any{"gcode_state": "FINISH", "stat": "A48000"}, 4, 0, 1, true},
		{map[string]any{"gcode_state": "FAILED", "stat": "248000"}, 5, 0, 0, true},
		{map[string]any{"gcode_state": "SOMETHING_NEW", "stat": "zz"}, -1, 0, 0, false},
	}
	for _, c := range cases {
		m := metrics(c.state)
		if m["state_code"] != c.code || m["active"] != c.active {
			t.Errorf("%v: state_code %v active %v, want %v %v", c.state, m["state_code"], m["active"], c.code, c.active)
		}
		door, has := m["door_open"]
		if has != c.hasDoor || door != c.door {
			t.Errorf("%v: door_open %v (present %v), want %v (present %v)", c.state, door, has, c.door, c.hasDoor)
		}
	}
	if m := metrics(map[string]any{}); len(m) != 0 {
		t.Errorf("empty state produced %v", m)
	}
}

func TestMetricsRejectsNonFinite(t *testing.T) {
	m := metrics(map[string]any{"bed_temper": "NaN", "nozzle_temper": "+Inf", "mc_percent": true})
	if len(m) != 0 {
		t.Errorf("got %v", m)
	}
}

func TestBuildPayload(t *testing.T) {
	state := map[string]any{"gcode_state": "RUNNING", "bed_temper": 60.0}
	up := decode(t, buildPayload(state, true, 1700000000123))
	if up["ts"] != 1700000000123 || up["printer_reachable"] != 1 || up["bed_c"] != 60 || up["active"] != 1 {
		t.Errorf("reachable payload: %v", up)
	}
	down := decode(t, buildPayload(state, false, 5))
	if len(down) != 2 || down["printer_reachable"] != 0 || down["ts"] != 5 {
		t.Errorf("unreachable payload must only carry ts + printer_reachable: %v", down)
	}
}

func TestChangeKey(t *testing.T) {
	a := map[string]any{"gcode_state": "RUNNING", "mc_percent": 1}
	b := map[string]any{"gcode_state": "RUNNING", "mc_percent": 2}
	if changeKey(a) != changeKey(b) {
		t.Error("progress alone must not count as a transition")
	}
	b["gcode_state"] = "FINISH"
	if changeKey(a) == changeKey(b) {
		t.Error("state change not detected")
	}
}

func TestParseSSDP(t *testing.T) {
	msg := "NOTIFY * HTTP/1.1\r\nHOST: 239.255.255.250:1990\r\nLocation: 192.168.68.60\r\n" +
		"NT: urn:bambulab-com:device:3dprinter:1\r\nUSN: 01P00A123456789\r\nDevModel.bambu.com: N7\r\n\r\n"
	h := parseSSDP([]byte(msg))
	if h["devmodel.bambu.com"] != "N7" || h["location"] != "192.168.68.60" {
		t.Errorf("headers: %v", h)
	}
	if !announces(h, "01p00a123456789") || announces(h, "other") || announces(h, "") {
		t.Error("serial matching wrong")
	}
}

func FuzzReport(f *testing.F) {
	f.Add([]byte(`{"print":{"gcode_state":"RUNNING","mc_percent":10,"stat":"A48000","bed_temper":"60.5"}}`))
	f.Add([]byte(`{"print":{"nozzle_temper":1e400,"layer_num":null}}`))
	f.Add([]byte(`{"print":{"stg_cur":[1],"print_error":{"a":1}}}`))
	f.Fuzz(func(t *testing.T, payload []byte) {
		state := map[string]any{}
		mergeReport(state, payload)
		for _, reachable := range []bool{true, false} {
			var out map[string]float64
			if err := json.Unmarshal(buildPayload(state, reachable, 1), &out); err != nil {
				t.Fatalf("payload not a flat numeric object: %v", err)
			}
		}
		_ = changeKey(state)
	})
}
