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
