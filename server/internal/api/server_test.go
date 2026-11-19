package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"maskinsjekk/internal/config"
	"maskinsjekk/internal/store"
)

var now = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func newTestServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s := New(st, config.Config{StaleThreshold: 30 * time.Second}, log.New(io.Discard, "", 0))
	s.now = func() time.Time { return now }
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, st
}

func get(t *testing.T, url string, out any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	if out != nil {
		_ = json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func seed(t *testing.T, st *store.Store, machine string, receivedAt time.Time, values ...float64) {
	t.Helper()
	var ms []store.Measurement
	for i, v := range values {
		ts := now.Add(-time.Duration(len(values)-i) * time.Second).UnixMilli()
		ms = append(ms, store.Measurement{SiteID: "demo", MachineID: machine, SensorID: "esp32-1", Metric: "vib_rms", Value: v, TS: ts, ReceivedAt: receivedAt.UnixMilli()})
	}
	if err := st.InsertMeasurements(context.Background(), ms); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestMachines(t *testing.T) {
	ts, st := newTestServer(t)

	var empty struct {
		Machines []machineJSON `json:"machines"`
	}
	if code := get(t, ts.URL+"/machines", &empty); code != 200 || empty.Machines == nil || len(empty.Machines) != 0 {
		t.Fatalf("empty list: code=%d %+v", code, empty)
	}

	seed(t, st, "fresh", now.Add(-5*time.Second), 0.1)
	seed(t, st, "old", now.Add(-time.Hour), 0.1)

	var got struct {
		Machines []machineJSON `json:"machines"`
	}
	get(t, ts.URL+"/machines", &got)
	if len(got.Machines) != 2 {
		t.Fatalf("got %+v", got)
	}
	byID := map[string]machineJSON{}
	for _, m := range got.Machines {
		byID[m.ID] = m
	}
	if byID["fresh"].Stale || !byID["old"].Stale {
		t.Fatalf("staleness wrong: %+v", got.Machines)
	}
	if len(byID["fresh"].Sensors) != 1 || byID["fresh"].Sensors[0].ID != "esp32-1" {
		t.Fatalf("sensors wrong: %+v", byID["fresh"])
	}
}

func TestSeries(t *testing.T) {
	ts, st := newTestServer(t)
	seed(t, st, "printer-1", now, 0.1, 0.2, 0.3)

	var got struct {
		MachineID string  `json:"machine_id"`
		Metric    string  `json:"metric"`
		Points    []point `json:"points"`
	}
	from := strconv.FormatInt(now.Add(-time.Hour).UnixMilli(), 10)
	if code := get(t, ts.URL+"/machines/printer-1/series?metric=vib_rms&from="+from, &got); code != 200 {
		t.Fatalf("code %d", code)
	}
	total := 0
	for _, p := range got.Points {
		total += p.N
	}
	if got.MachineID != "printer-1" || got.Metric != "vib_rms" || total != 3 {
		t.Fatalf("unexpected: %+v", got)
	}

	// RFC3339 range + raw (sub-minute) buckets.
	url := ts.URL + "/machines/printer-1/series?metric=vib_rms&bucket=1&from=" +
		now.Add(-10*time.Second).Format(time.RFC3339) + "&to=" + now.Format(time.RFC3339)
	get(t, url, &got)
	if len(got.Points) != 3 {
		t.Fatalf("raw series: %+v", got.Points)
	}
}

func TestSeriesErrors(t *testing.T) {
	ts, st := newTestServer(t)
	seed(t, st, "printer-1", now, 0.1)

	cases := map[string]int{
		"/machines/printer-1/series":                                             400, // no metric
		"/machines/nope/series?metric=vib_rms":                                   404,
		"/machines/printer-1/series?metric=vib_rms&from=yesterday":               400,
		"/machines/printer-1/series?metric=vib_rms&bucket=0":                     400,
		"/machines/printer-1/series?metric=vib_rms&from=2000&to=1000":            400, // from after to
		"/machines/printer-1/series?metric=vib_rms&bucket=1&from=0&to=100000000": 400, // > 5000 points
		"/machines/printer-1/series?metric=vib_rms&bucket=1&from=0&to=1000":      200,
	}

	for path, want := range cases {
		if code := get(t, ts.URL+path, nil); code != want {
			t.Errorf("%s: code %d, want %d", path, code, want)
		}
	}
}

func TestHealthAndCORS(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("healthz: %d cors=%q", resp.StatusCode, resp.Header.Get("Access-Control-Allow-Origin"))
	}
	if code := get(t, ts.URL+"/nope", nil); code != 404 {
		t.Fatalf("unknown path: %d", code)
	}
}

func TestIndexPage(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("index: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if r2, err := http.Get(ts.URL + "/nope"); err != nil || r2.StatusCode != 404 {
		t.Fatalf("unknown path should 404, got %v %v", r2, err)
	}
}

func TestStateAndTimeline(t *testing.T) {
	ts, st := newTestServer(t)
	seed(t, st, "printer-1", now, 0.1)
	ctx := context.Background()

	var cur struct {
		State   *string `json:"state"`
		Since   *string `json:"since"`
		SinceTS *int64  `json:"since_ts"`
		Reason  string  `json:"reason"`
	}
	if code := get(t, ts.URL+"/machines/printer-1/state", &cur); code != 200 || cur.State != nil {
		t.Fatalf("no state yet: code=%d %+v", code, cur)
	}

	h := func(n int) int64 { return now.Add(time.Duration(n) * time.Hour).UnixMilli() }
	for _, ev := range []struct {
		state string
		start int64
	}{{"OFF", h(-30)}, {"IDLE", h(-5)}, {"RUNNING", h(-2)}} {
		if err := st.RecordState(ctx, "printer-1", ev.state, ev.start, "power=150"); err != nil {
			t.Fatal(err)
		}
	}

	get(t, ts.URL+"/machines/printer-1/state", &cur)
	if cur.State == nil || *cur.State != "RUNNING" || *cur.SinceTS != h(-2) || cur.Reason != "power=150" {
		t.Fatalf("state: %+v", cur)
	}

	var tl struct {
		Segments []segment `json:"segments"`
	}
	// Default range (last 24 h): OFF clipped to the range start, RUNNING open.
	if code := get(t, ts.URL+"/machines/printer-1/timeline", &tl); code != 200 || len(tl.Segments) != 3 {
		t.Fatalf("timeline: code=%d %+v", code, tl)
	}
	s := tl.Segments
	if s[0].State != "OFF" || s[0].StartTS != h(-24) || *s[0].EndTS != h(-5) {
		t.Errorf("first segment not clipped: %+v", s[0])
	}
	if s[2].State != "RUNNING" || s[2].End != nil || s[2].EndTS != nil {
		t.Errorf("ongoing segment: %+v", s[2])
	}

	// A range ending inside IDLE clips its end and leaves RUNNING out.
	url := ts.URL + "/machines/printer-1/timeline?from=" + strconv.FormatInt(h(-6), 10) + "&to=" + strconv.FormatInt(h(-3), 10)
	get(t, url, &tl)
	if len(tl.Segments) != 2 || tl.Segments[1].State != "IDLE" || *tl.Segments[1].EndTS != h(-3) {
		t.Errorf("clipped range: %+v", tl.Segments)
	}

	cases := map[string]int{
		"/machines/nope/state":                         404,
		"/machines/nope/timeline":                      404,
		"/machines/printer-1/timeline?from=yesterday":  400,
		"/machines/printer-1/timeline?from=2000&to=10": 400,
	}
	for path, want := range cases {
		if code := get(t, ts.URL+path, nil); code != want {
			t.Errorf("%s: code %d, want %d", path, code, want)
		}
	}
}

func TestJobsAndRuleSets(t *testing.T) {
	ts, st := newTestServer(t)
	seed(t, st, "printer-1", now, 0.1)
	ctx := context.Background()
	m := func(n int) int64 { return now.Add(time.Duration(n) * time.Minute).UnixMilli() }
	for _, ev := range []struct {
		state string
		start int64
	}{{"IDLE", m(-120)}, {"HEATING", m(-100)}, {"RUNNING", m(-98)}, {"IDLE", m(-40)}, {"HEATING", m(-5)}} {
		if err := st.RecordState(ctx, "printer-1", ev.state, ev.start, ""); err != nil {
			t.Fatal(err)
		}
	}

	var got struct {
		Jobs []jobJSON `json:"jobs"`
	}
	if code := get(t, ts.URL+"/machines/printer-1/jobs", &got); code != 200 || len(got.Jobs) != 2 {
		t.Fatalf("jobs: code=%d %+v", code, got)
	}
	cur, done := got.Jobs[0], got.Jobs[1] // newest first
	if cur.EndTS != nil || cur.StartTS != m(-5) || cur.DurationS != 300 {
		t.Errorf("ongoing job: %+v", cur)
	}
	if done.StartTS != m(-100) || *done.EndTS != m(-40) || done.DurationS != 3600 || done.EnergyWh != nil {
		t.Errorf("finished job: %+v", done)
	}
	// A range that ends before the second job.
	url := ts.URL + "/machines/printer-1/jobs?from=" + strconv.FormatInt(m(-110), 10) + "&to=" + strconv.FormatInt(m(-30), 10)
	if get(t, url, &got); len(got.Jobs) != 1 {
		t.Errorf("ranged jobs: %+v", got.Jobs)
	}

	var sets struct {
		RuleSets []ruleSetJSON `json:"rule_sets"`
	}
	if code := get(t, ts.URL+"/machines/printer-1/rulesets", &sets); code != 200 || sets.RuleSets == nil || len(sets.RuleSets) != 0 {
		t.Fatalf("rulesets: code=%d %+v", code, sets)
	}
	if _, _, err := st.ActivateRuleSet(ctx, "printer-1", "abc", `{"rules":[]}`, m(0)); err != nil {
		t.Fatal(err)
	}
	get(t, ts.URL+"/machines/printer-1/rulesets", &sets)
	if len(sets.RuleSets) != 2 || !sets.RuleSets[0].Current || sets.RuleSets[1].Archived == nil || string(sets.RuleSets[1].Config) != "null" {
		t.Fatalf("rulesets after activation: %+v", sets.RuleSets)
	}
	// The archived (pre-rule-set) timeline stays readable.
	var tl struct {
		Segments []segment `json:"segments"`
	}
	get(t, ts.URL+"/machines/printer-1/timeline?rules="+strconv.FormatInt(sets.RuleSets[1].ID, 10), &tl)
	if len(tl.Segments) != 5 {
		t.Errorf("archived timeline: %+v", tl.Segments)
	}
	get(t, ts.URL+"/machines/printer-1/timeline", &tl)
	if len(tl.Segments) != 0 {
		t.Errorf("current timeline right after activation: %+v", tl.Segments)
	}

	for path, want := range map[string]int{
		"/machines/nope/jobs":                    404,
		"/machines/nope/rulesets":                404,
		"/machines/printer-1/timeline?rules=x":   400,
		"/machines/printer-1/jobs?from=tomorrow": 400,
	} {
		if code := get(t, ts.URL+path, nil); code != want {
			t.Errorf("%s: code %d, want %d", path, code, want)
		}
	}
}
