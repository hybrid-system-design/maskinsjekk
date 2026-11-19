// Package api serves the HTTP JSON endpoints: the machine registry,
// raw/downsampled metric series, detected machine states (current, timeline,
// archived rule sets) and jobs. A metrics endpoint arrives with the dashboard
// (Phase 4).
// Responses include permissive CORS so a browser dashboard can read them.
package api

import (
	_ "embed"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"maskinsjekk/internal/config"
	"maskinsjekk/internal/state"
	"maskinsjekk/internal/store"
)

// maxSeriesPoints caps one series response so a wide range with a tiny bucket
// can't produce an unbounded payload.
const maxSeriesPoints = 5000

// indexHTML is the minimal live view served at "/" until the Phase 4 dashboard.
//
//go:embed web/index.html
var indexHTML []byte

// Server holds the dependencies for the HTTP handlers.
type Server struct {
	store *store.Store
	cfg   config.Config
	log   *log.Logger
	now   func() time.Time
}

// New constructs a Server.
func New(st *store.Store, cfg config.Config, logger *log.Logger) *Server {
	return &Server{store: st, cfg: cfg, log: logger, now: time.Now}
}

// Handler returns the fully wired http.Handler (routes + CORS).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /machines", s.handleMachines)
	mux.HandleFunc("GET /machines/{id}/series", s.handleSeries)
	mux.HandleFunc("GET /machines/{id}/state", s.handleState)
	mux.HandleFunc("GET /machines/{id}/timeline", s.handleTimeline)
	mux.HandleFunc("GET /machines/{id}/rulesets", s.handleRuleSets)
	mux.HandleFunc("GET /machines/{id}/jobs", s.handleJobs)
	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /{$}", handleIndex)
	return cors(mux)
}

func handleIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}

type sensorJSON struct {
	ID       string  `json:"id"`
	Kind     string  `json:"kind"`
	LastSeen *string `json:"last_seen"`
}

type machineJSON struct {
	ID       string       `json:"id"`
	SiteID   string       `json:"site_id"`
	Name     string       `json:"name"`
	LastSeen *string      `json:"last_seen"` // null if never seen
	Stale    bool         `json:"stale"`     // no data within STALE_THRESHOLD
	Sensors  []sensorJSON `json:"sensors"`
}

func (s *Server) handleMachines(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.Machines(r.Context())
	if err != nil {
		s.log.Printf("api: machines: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	nowMS := s.now().UnixMilli()
	out := make([]machineJSON, 0, len(list))
	for _, m := range list {
		mj := machineJSON{
			ID:       m.ID,
			SiteID:   m.SiteID,
			Name:     m.Name,
			LastSeen: msToRFC3339(m.LastSeenMS),
			Stale:    m.LastSeenMS == 0 || nowMS-m.LastSeenMS > s.cfg.StaleThreshold.Milliseconds(),
			Sensors:  make([]sensorJSON, 0, len(m.Sensors)),
		}
		for _, sn := range m.Sensors {
			mj.Sensors = append(mj.Sensors, sensorJSON{ID: sn.ID, Kind: sn.Kind, LastSeen: msToRFC3339(sn.LastSeenMS)})
		}
		out = append(out, mj)
	}
	writeJSON(w, http.StatusOK, map[string]any{"machines": out})
}

type point struct {
	T   string  `json:"t"`  // RFC3339 bucket start
	TS  int64   `json:"ts"` // unix ms bucket start
	Avg float64 `json:"avg"`
	Min float64 `json:"min"`
	Max float64 `json:"max"`
	N   int     `json:"n"`
}

// handleSeries: GET /machines/{id}/series?metric=vib_rms&from=&to=&bucket=
// from/to are RFC3339 or unix ms (default: last 24h); bucket is seconds
// (default 60).
func (s *Server) handleSeries(w http.ResponseWriter, r *http.Request) {
	machineID := r.PathValue("id")
	q := r.URL.Query()
	metric := q.Get("metric")
	if !config.ValidID(metric) {
		writeErr(w, http.StatusBadRequest, "metric is required")
		return
	}
	if !s.machineKnown(w, r, machineID) {
		return
	}

	nowMS := s.now().UnixMilli()
	toMS, ok := parseTime(q.Get("to"), nowMS)
	if !ok {
		writeErr(w, http.StatusBadRequest, "to must be RFC3339 or unix ms")
		return
	}
	fromMS, ok := parseTime(q.Get("from"), toMS-24*time.Hour.Milliseconds())
	if !ok {
		writeErr(w, http.StatusBadRequest, "from must be RFC3339 or unix ms")
		return
	}
	if fromMS >= toMS {
		writeErr(w, http.StatusBadRequest, "from must be before to")
		return
	}
	bucketSec, err := strconv.ParseInt(defaultStr(q.Get("bucket"), "60"), 10, 64)
	if err != nil || bucketSec <= 0 {
		writeErr(w, http.StatusBadRequest, "bucket must be a positive number of seconds")
		return
	}
	bucketMS := bucketSec * 1000
	if (toMS-fromMS)/bucketMS > maxSeriesPoints {
		writeErr(w, http.StatusBadRequest, "range/bucket would exceed 5000 points; use a larger bucket")
		return
	}

	buckets, err := s.store.Series(r.Context(), machineID, metric, fromMS, toMS, bucketMS)
	if err != nil {
		s.log.Printf("api: series %s/%s: %v", machineID, metric, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	points := make([]point, 0, len(buckets))
	for _, b := range buckets {
		points = append(points, point{
			T: rfc3339(b.T), TS: b.T,
			Avg: b.Avg, Min: b.Min, Max: b.Max, N: b.N,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"machine_id":     machineID,
		"metric":         metric,
		"bucket_seconds": bucketSec,
		"from":           rfc3339(fromMS),
		"to":             rfc3339(toMS),
		"points":         points,
	})
}

// machineKnown writes a 404 (or 500) and returns false unless the machine exists.
func (s *Server) machineKnown(w http.ResponseWriter, r *http.Request, machineID string) bool {
	known, err := s.store.MachineExists(r.Context(), machineID)
	if err != nil {
		s.log.Printf("api: machine %s: %v", machineID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return false
	}
	if !known {
		writeErr(w, http.StatusNotFound, "machine not found")
		return false
	}
	return true
}

// handleState: GET /machines/{id}/state — the current detected state; state
// is null for a machine without state rules (or before the first evaluation).
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	machineID := r.PathValue("id")
	if !s.machineKnown(w, r, machineID) {
		return
	}
	ev, ok, err := s.store.CurrentState(r.Context(), machineID)
	if err != nil {
		s.log.Printf("api: state %s: %v", machineID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := map[string]any{"machine_id": machineID, "state": nil, "since": nil, "since_ts": nil, "reason": ""}
	if ok {
		out["state"], out["since"], out["since_ts"], out["reason"] = ev.State, rfc3339(ev.Start), ev.Start, ev.Reason
	}
	writeJSON(w, http.StatusOK, out)
}

type segment struct {
	State   string  `json:"state"`
	Start   string  `json:"start"`
	StartTS int64   `json:"start_ts"`
	End     *string `json:"end"`    // null while ongoing
	EndTS   *int64  `json:"end_ts"` // null while ongoing
}

// parseRange reads from/to (RFC3339 or unix ms; default the last 24h) and
// writes a 400 and returns false if they are invalid.
func (s *Server) parseRange(w http.ResponseWriter, r *http.Request) (fromMS, toMS int64, ok bool) {
	q := r.URL.Query()
	toMS, ok = parseTime(q.Get("to"), s.now().UnixMilli())
	if !ok {
		writeErr(w, http.StatusBadRequest, "to must be RFC3339 or unix ms")
		return 0, 0, false
	}
	fromMS, ok = parseTime(q.Get("from"), toMS-24*time.Hour.Milliseconds())
	if !ok {
		writeErr(w, http.StatusBadRequest, "from must be RFC3339 or unix ms")
		return 0, 0, false
	}
	if fromMS >= toMS {
		writeErr(w, http.StatusBadRequest, "from must be before to")
		return 0, 0, false
	}
	return fromMS, toMS, true
}

// handleTimeline: GET /machines/{id}/timeline?from=&to=&rules= — state
// intervals overlapping the range (default: last 24h), clipped to it. The
// ongoing state has end null. rules picks an archived rule set (see
// /rulesets); default is the current one.
func (s *Server) handleTimeline(w http.ResponseWriter, r *http.Request) {
	machineID := r.PathValue("id")
	fromMS, toMS, ok := s.parseRange(w, r)
	if !ok {
		return
	}
	var ruleSet int64
	if v := r.URL.Query().Get("rules"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n <= 0 {
			writeErr(w, http.StatusBadRequest, "rules must be a rule set id")
			return
		}
		ruleSet = n
	}
	if !s.machineKnown(w, r, machineID) {
		return
	}
	evs, err := s.store.StateEvents(r.Context(), machineID, ruleSet, fromMS, toMS)
	if err != nil {
		s.log.Printf("api: timeline %s: %v", machineID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	segs := make([]segment, 0, len(evs))
	for _, ev := range evs {
		start := max(ev.Start, fromMS)
		sg := segment{State: ev.State, Start: rfc3339(start), StartTS: start}
		if ev.End != 0 {
			end := min(ev.End, toMS)
			sg.End, sg.EndTS = msToRFC3339(end), &end
		}
		segs = append(segs, sg)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"machine_id": machineID,
		"from":       rfc3339(fromMS),
		"to":         rfc3339(toMS),
		"segments":   segs,
	})
}

type ruleSetJSON struct {
	ID       int64           `json:"id"`
	Current  bool            `json:"current"`
	Created  string          `json:"created"`
	Archived *string         `json:"archived"` // null for the current rule set
	Config   json.RawMessage `json:"config"`   // null for events from before rule sets
}

// handleRuleSets: GET /machines/{id}/rulesets — the state rule sets the
// machine has run with, newest first. When the rules change, the timeline
// detected under the old ones is archived and stays readable via
// /timeline?rules=<id>.
func (s *Server) handleRuleSets(w http.ResponseWriter, r *http.Request) {
	machineID := r.PathValue("id")
	if !s.machineKnown(w, r, machineID) {
		return
	}
	sets, err := s.store.RuleSets(r.Context(), machineID)
	if err != nil {
		s.log.Printf("api: rulesets %s: %v", machineID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	out := make([]ruleSetJSON, 0, len(sets))
	for _, rs := range sets {
		j := ruleSetJSON{ID: rs.ID, Current: rs.ArchivedAt == 0, Created: rfc3339(rs.CreatedAt),
			Archived: msToRFC3339(rs.ArchivedAt), Config: json.RawMessage("null")}
		if rs.Config != "" {
			j.Config = json.RawMessage(rs.Config)
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string]any{"machine_id": machineID, "rule_sets": out})
}

type jobJSON struct {
	Start     string   `json:"start"`
	StartTS   int64    `json:"start_ts"`
	End       *string  `json:"end"`    // null while running
	EndTS     *int64   `json:"end_ts"` // null while running
	DurationS int64    `json:"duration_s"`
	EnergyWh  *float64 `json:"energy_wh"` // null without power data
}

// jobLookback: a job overlapping the range may have started this long before
// it; its real start is still reported.
const jobLookback = 24 * time.Hour

// handleJobs: GET /machines/{id}/jobs?from=&to= — jobs (stretches of
// HEATING/RUNNING under the current rules) overlapping the range, newest
// first, with duration and energy.
func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	machineID := r.PathValue("id")
	fromMS, toMS, ok := s.parseRange(w, r)
	if !ok {
		return
	}
	if !s.machineKnown(w, r, machineID) {
		return
	}
	evs, err := s.store.StateEvents(r.Context(), machineID, 0, fromMS-jobLookback.Milliseconds(), toMS)
	if err != nil {
		s.log.Printf("api: jobs %s: %v", machineID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	nowMS := s.now().UnixMilli()
	jobs := state.Jobs(evs)
	out := make([]jobJSON, 0, len(jobs))
	for i := len(jobs) - 1; i >= 0; i-- {
		j := jobs[i]
		end := j.End
		if end == 0 {
			end = nowMS
		}
		if end <= fromMS || j.Start >= toMS {
			continue
		}
		jj := jobJSON{Start: rfc3339(j.Start), StartTS: j.Start, DurationS: (end - j.Start) / 1000}
		if j.End != 0 {
			e := j.End
			jj.End, jj.EndTS = msToRFC3339(e), &e
		}
		wh, ok, err := s.store.EnergyWh(r.Context(), machineID, j.Start, end)
		if err != nil {
			s.log.Printf("api: jobs %s: %v", machineID, err)
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		if ok {
			jj.EnergyWh = &wh
		}
		out = append(out, jj)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"machine_id": machineID,
		"from":       rfc3339(fromMS),
		"to":         rfc3339(toMS),
		"jobs":       out,
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// --- helpers ---

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// parseTime accepts RFC3339 or unix milliseconds; empty yields def.
func parseTime(s string, def int64) (int64, bool) {
	if s == "" {
		return def, true
	}
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UnixMilli(), true
	}
	return 0, false
}

func defaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// msToRFC3339 formats unix ms, or nil for 0 (never).
func msToRFC3339(ms int64) *string {
	if ms == 0 {
		return nil
	}
	s := rfc3339(ms)
	return &s
}

// rfc3339 formats unix ms as RFC3339 UTC.
func rfc3339(ms int64) string { return time.UnixMilli(ms).UTC().Format(time.RFC3339) }
