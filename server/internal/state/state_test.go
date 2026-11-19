package state

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"maskinsjekk/internal/config"
)

func f(v float64) *float64 { return &v }

func dur(d time.Duration) config.Duration { return config.Duration(d) }

// testConfig: power primary (required), vibration optional.
func testConfig() *config.StateConfig {
	return &config.StateConfig{
		Signals: map[string]config.Signal{
			"power": {Metric: "power_w", Window: dur(time.Minute), MaxAge: dur(10 * time.Minute)},
			"vib":   {Metric: "vib_rms", Window: dur(time.Minute), MaxAge: dur(2 * time.Minute), Optional: true},
		},
		Rules: []config.StateRule{
			{State: config.StateOff, When: map[string]config.Range{"power": {Below: f(3)}}, EnterAfter: dur(time.Minute)},
			{State: config.StateIdle, When: map[string]config.Range{"power": {Below: f(20)}}, EnterAfter: dur(time.Minute)},
			{State: config.StateHeating, When: map[string]config.Range{"vib": {Below: f(0.02)}}, EnterAfter: dur(time.Minute)},
			{State: config.StateRunning, EnterAfter: dur(time.Minute)},
		},
	}
}

func TestSignalValue(t *testing.T) {
	sig := config.Signal{Window: dur(time.Minute), MaxAge: dur(4 * time.Minute)}
	samples := []Sample{{0, 100}, {200_000, 1}, {230_000, 3}, {500_000, 99}}
	cases := []struct {
		now  int64
		want float64
		ok   bool
	}{
		{-1, 0, false},      // before any sample
		{0, 100, true},      // the sample itself
		{240_000, 2, true},  // mean of the two in the window
		{260_000, 3, true},  // only the newer one left in the window
		{400_000, 3, true},  // window empty: latest within max_age
		{480_000, 0, false}, // future sample ignored; latest (230 s) too old
		{559_999, 99, true}, // last sample still in the window
	}
	for _, c := range cases {
		v, ok := SignalValue(samples, c.now, sig)
		if ok != c.ok || (ok && v != c.want) {
			t.Errorf("now=%d: got %v,%v want %v,%v", c.now, v, ok, c.want, c.ok)
		}
	}
}

func TestClassify(t *testing.T) {
	cfg := testConfig()
	cases := []struct {
		vals Values
		want string
	}{
		{Values{"power": 0, "vib": 0.01}, config.StateOff},
		{Values{"power": 3, "vib": 0.05}, config.StateIdle}, // power wins over vibration
		{Values{"power": 150, "vib": 0.01}, config.StateHeating},
		{Values{"power": 150, "vib": 0.04}, config.StateRunning},
		{Values{"power": 150}, config.StateRunning}, // optional vib missing: HEATING rule skipped
		{Values{"vib": 0.04}, config.StateUnknown},  // required power missing
		{Values{}, config.StateUnknown},
	}
	for _, c := range cases {
		if got, reason := Classify(cfg, c.vals); got != c.want {
			t.Errorf("%v: got %s (%s), want %s", c.vals, got, reason, c.want)
		}
	}
	if _, reason := Classify(cfg, Values{"power": 152.34}); reason != "power=152.3 vib=-" {
		t.Errorf("reason = %q", reason)
	}

	noDefault := testConfig()
	noDefault.Rules = noDefault.Rules[:1]
	if got, _ := Classify(noDefault, Values{"power": 50}); got != config.StateUnknown {
		t.Errorf("no matching rule: got %s", got)
	}
}

func TestTrackerDebounce(t *testing.T) {
	tr := NewTracker(testConfig())
	step := func(now int64, cand string) (Transition, bool) { return tr.Step(now, cand, "r") }

	if got, ok := step(0, config.StateOff); !ok || got.State != config.StateOff || got.Start != 0 {
		t.Fatalf("first step: %+v %v", got, ok)
	}
	// A 30 s blip of RUNNING is ignored.
	step(10_000, config.StateRunning)
	step(40_000, config.StateRunning)
	if _, ok := step(50_000, config.StateOff); ok {
		t.Fatal("blip caused a transition")
	}
	// Switching candidates restarts the debounce.
	step(60_000, config.StateHeating)
	if _, ok := step(100_000, config.StateRunning); ok {
		t.Fatal("transition before debounce")
	}
	if _, ok := step(159_999, config.StateRunning); ok {
		t.Fatal("transition before debounce")
	}
	got, ok := step(160_000, config.StateRunning)
	if !ok || got.State != config.StateRunning || got.Start != 100_000 {
		t.Fatalf("debounced transition: %+v %v (want RUNNING backdated to 100000)", got, ok)
	}
	if s, since := tr.Current(); s != config.StateRunning || since != 100_000 {
		t.Fatalf("current = %s %d", s, since)
	}
	// UNKNOWN is entered at once.
	if got, ok := step(170_000, config.StateUnknown); !ok || got.Start != 170_000 {
		t.Fatalf("unknown: %+v %v", got, ok)
	}
}

func TestTrackerResume(t *testing.T) {
	tr := NewTracker(testConfig())
	tr.Resume(config.StateRunning, 1_000_000)
	if _, ok := tr.Step(2_000_000, config.StateRunning, ""); ok {
		t.Fatal("same state after resume caused a transition")
	}
	tr.Step(2_100_000, config.StateIdle, "")
	if got, ok := tr.Step(2_160_000, config.StateIdle, ""); !ok || got.Start != 2_100_000 {
		t.Fatalf("got %+v %v", got, ok)
	}
}

// loadFixture reads a "ts_ms,metric,value" CSV into per-metric samples.
func loadFixture(t *testing.T, path string) map[string][]Sample {
	t.Helper()
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	out := map[string][]Sample{}
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "ts_ms") {
			continue
		}
		parts := strings.Split(line, ",")
		ts, err1 := strconv.ParseInt(parts[0], 10, 64)
		v, err2 := strconv.ParseFloat(parts[2], 64)
		if len(parts) != 3 || err1 != nil || err2 != nil {
			t.Fatalf("bad fixture line %q", line)
		}
		out[parts[1]] = append(out[parts[1]], Sample{ts, v})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// machineState returns a machine's state config from the shipped machines file.
func machineState(t *testing.T, id string) *config.StateConfig {
	t.Helper()
	m, err := config.LoadMachines("../../../deploy/config/machines.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, mc := range m.StateMachines() {
		if mc.ID == id {
			return mc.State
		}
	}
	t.Fatalf("machine %s has no state config", id)
	return nil
}

// TestReplayRealPrints runs the shipped printer-1 rules over two real prints
// and checks them against the printer's own status (Bambu "active": a job in
// progress, which our HEATING + RUNNING should cover).
func TestReplayRealPrints(t *testing.T) {
	cfg := machineState(t, "printer-1")
	data := loadFixture(t, "testdata/printer-2026-11-13.csv")
	series := map[string][]Sample{}
	for name, sig := range cfg.Signals {
		series[name] = data[sig.Metric]
	}
	from := data["power_w"][0].TS
	to := data["power_w"][len(data["power_w"])-1].TS
	const step = 10_000
	trs := Replay(cfg, series, from, to, step)
	for _, tr := range trs {
		t.Logf("%s %-8s %s", time.UnixMilli(tr.Start).UTC().Format("15:04:05"), tr.State, tr.Reason)
	}

	// Two jobs: each must show up as exactly one HEATING/RUNNING stretch.
	jobs := 0
	busy := func(s string) bool { return s == config.StateHeating || s == config.StateRunning }
	prev := ""
	for _, tr := range trs {
		if busy(tr.State) && !busy(prev) {
			jobs++
		}
		prev = tr.State
	}
	if jobs != 2 {
		t.Errorf("detected %d jobs, want 2", jobs)
	}

	// Minute-level agreement with the printer's own "job in progress" flag.
	stateAt := func(ts int64) string {
		s := ""
		for _, tr := range trs {
			if tr.Start > ts {
				break
			}
			s = tr.State
		}
		return s
	}
	agree, total := 0, 0
	for _, a := range data["active"] {
		if a.Value != 0 && a.Value != 1 {
			continue // bucket holding a transition
		}
		total++
		if busy(stateAt(a.TS)) == (a.Value == 1) {
			agree++
		}
	}
	pct := 100 * float64(agree) / float64(total)
	t.Logf("agreement with Bambu active: %d/%d = %.1f%%", agree, total, pct)
	if pct < 97 {
		t.Errorf("agreement %.1f%% < 97%%", pct)
	}
}
