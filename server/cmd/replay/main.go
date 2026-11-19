// Command replay runs a machine's state rules over stored history and prints
// the detected timeline, so thresholds can be tuned against real data before
// they go live. It reads the history over the HTTP API of a running service,
// and the rules from a machines file (edit a local copy, replay, repeat).
//
// Optionally it scores the result against a ground-truth metric that is 1
// while the machine is busy (e.g. the Bambu bridge's "active"): a detected
// HEATING or RUNNING counts as busy.
//
//	go run ./cmd/replay --url http://fedora.local:8080 --machine printer-1 \
//	    --machines ../deploy/config/machines.json --from 2026-11-13T12:00:00Z --truth active
//
// The series API averages a metric across all of a machine's sensors, so a
// signal's "sensor" is ignored here. Raw data is kept for RAW_RETENTION (7 days
// by default); older history only has per-minute rollups, so use --bucket 60.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"time"

	"maskinsjekk/internal/config"
	"maskinsjekk/internal/state"
)

func main() {
	base := flag.String("url", "http://localhost:8080", "base URL of the maskinsjekk service")
	machineID := flag.String("machine", "", "machine id (required)")
	machinesPath := flag.String("machines", "machines.json", "machines file with the state rules to test")
	fromS := flag.String("from", "", "start, RFC3339 (default: 24h before --to)")
	toS := flag.String("to", "", "end, RFC3339 (default: now)")
	bucket := flag.Int("bucket", 10, "series resolution in seconds (= evaluation step)")
	truth := flag.String("truth", "", "optional ground-truth metric: 1 while busy, 0 otherwise")
	quiet := flag.Bool("q", false, "summary only, no transition list")
	flag.Parse()

	if *machineID == "" {
		log.Fatal("--machine is required")
	}
	if *bucket <= 0 {
		log.Fatal("--bucket must be positive")
	}
	to := time.Now()
	if *toS != "" {
		to = mustTime(*toS)
	}
	from := to.Add(-24 * time.Hour)
	if *fromS != "" {
		from = mustTime(*fromS)
	}
	if !from.Before(to) {
		log.Fatal("--from must be before --to")
	}

	machines, err := config.LoadMachines(*machinesPath)
	if err != nil {
		log.Fatal(err)
	}
	var cfg *config.StateConfig
	for _, mc := range machines.StateMachines() {
		if mc.ID == *machineID {
			cfg = mc.State
		}
	}
	if cfg == nil {
		log.Fatalf("machine %q has no state rules in %s", *machineID, *machinesPath)
	}

	c := client{base: *base, machine: *machineID, bucket: *bucket}
	series := map[string][]state.Sample{}
	// Fetch from earlier than --from so windows are full at the start.
	var lookback time.Duration
	for _, sig := range cfg.Signals {
		lookback = max(lookback, max(sig.Window, sig.MaxAge).D())
	}
	for name, sig := range cfg.Signals {
		if series[name], err = c.series(sig.Metric, from.Add(-lookback), to); err != nil {
			log.Fatal(err)
		}
		fmt.Printf("signal %-8s %-12s %6d points\n", name, sig.Metric, len(series[name]))
	}

	stepMS := int64(*bucket) * 1000
	trs := state.Replay(cfg, series, from.UnixMilli(), to.UnixMilli(), stepMS)

	// Time per state, and agreement with the ground truth.
	end := to.UnixMilli()
	totals := map[string]int64{}
	for i, tr := range trs {
		next := end
		if i+1 < len(trs) {
			next = trs[i+1].Start
		}
		totals[tr.State] += next - tr.Start
		if !*quiet {
			fmt.Printf("%s  %-8s %8s  %s\n", time.UnixMilli(tr.Start).Local().Format("2006-01-02 15:04:05"),
				tr.State, fmtDur(next-tr.Start), tr.Reason)
		}
	}
	fmt.Printf("\n%d transitions from %s to %s\n", len(trs), from.Local().Format(time.DateTime), to.Local().Format(time.DateTime))
	names := make([]string, 0, len(totals))
	for s := range totals {
		names = append(names, s)
	}
	sort.Strings(names)
	for _, s := range names {
		fmt.Printf("  %-8s %10s\n", s, fmtDur(totals[s]))
	}

	if *truth != "" {
		gt, err := c.series(*truth, from, to)
		if err != nil {
			log.Fatal(err)
		}
		score(trs, gt)
	}
}

// score prints a confusion matrix of detected busy (HEATING/RUNNING) against
// the ground-truth metric, per bucket. Buckets where the truth changed (a
// value strictly between 0 and 1) and UNKNOWN stretches are left out.
func score(trs []state.Transition, gt []state.Sample) {
	var tp, tn, fp, fn, skipped int
	i := -1
	for _, g := range gt {
		for i+1 < len(trs) && trs[i+1].Start <= g.TS {
			i++
		}
		if i < 0 || trs[i].State == config.StateUnknown || (g.Value != 0 && g.Value != 1) {
			skipped++
			continue
		}
		busy := trs[i].State == config.StateHeating || trs[i].State == config.StateRunning
		switch {
		case busy && g.Value == 1:
			tp++
		case !busy && g.Value == 0:
			tn++
		case busy:
			fp++
		default:
			fn++
		}
	}
	n := tp + tn + fp + fn
	if n == 0 {
		fmt.Println("\nno ground truth in range")
		return
	}
	fmt.Printf("\nground truth (%d buckets, %d skipped):\n", n, skipped)
	fmt.Printf("                truth busy  truth not busy\n")
	fmt.Printf("  detected busy %10d  %14d\n", tp, fp)
	fmt.Printf("  detected not  %10d  %14d\n", fn, tn)
	fmt.Printf("  agreement %.1f%%\n", 100*float64(tp+tn)/float64(n))
}

type client struct {
	base    string
	machine string
	bucket  int
}

// series fetches a metric's bucket averages, in chunks within the API's
// 5000-point limit.
func (c client) series(metric string, from, to time.Time) ([]state.Sample, error) {
	chunk := time.Duration(c.bucket) * time.Second * 4000
	var out []state.Sample
	for start := from; start.Before(to); start = start.Add(chunk) {
		end := start.Add(chunk)
		if end.After(to) {
			end = to
		}
		q := url.Values{
			"metric": {metric},
			"bucket": {strconv.Itoa(c.bucket)},
			"from":   {strconv.FormatInt(start.UnixMilli(), 10)},
			"to":     {strconv.FormatInt(end.UnixMilli(), 10)},
		}
		u := c.base + "/machines/" + url.PathEscape(c.machine) + "/series?" + q.Encode()
		resp, err := http.Get(u)
		if err != nil {
			return nil, err
		}
		var body struct {
			Error  string `json:"error"`
			Points []struct {
				TS  int64   `json:"ts"`
				Avg float64 `json:"avg"`
			} `json:"points"`
		}
		err = json.NewDecoder(resp.Body).Decode(&body)
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", metric, err)
		}
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("%s: %s: %s", metric, resp.Status, body.Error)
		}
		for _, p := range body.Points {
			out = append(out, state.Sample{TS: p.TS, Value: p.Avg})
		}
	}
	return out, nil
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		fmt.Fprintf(os.Stderr, "bad time %q (want RFC3339, e.g. 2026-11-13T12:00:00Z)\n", s)
		os.Exit(2)
	}
	return t
}

func fmtDur(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	if d >= time.Hour {
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return d.Round(time.Second).String()
}
