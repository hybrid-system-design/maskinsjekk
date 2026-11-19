// Package store is the SQLite persistence layer.
//
// The model is generic: one customer = one site with many machines, each with
// any number of sensors producing named metrics. Raw measurements (≈1 Hz) are
// kept for a limited time; per-minute rollups are kept indefinitely. State
// changes and jobs are stored as events, not derived on read.
//
// All timestamps are unix milliseconds (UTC).
//
// It uses the pure-Go driver modernc.org/sqlite (no cgo) so the service builds
// as a fully static binary suitable for a scratch container image.
package store

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"maskinsjekk/internal/config"
)

// Store wraps the SQLite database connection.
type Store struct {
	db *sql.DB
}

// Measurement is one normalized value: (machine, sensor, metric, value, ts).
type Measurement struct {
	SiteID     string
	MachineID  string
	SensorID   string
	Metric     string
	Value      float64
	TS         int64 // canonical series time, unix ms
	ReceivedAt int64 // server arrival time, unix ms
}

// MachineInfo is a machine with its registry data and liveness.
type MachineInfo struct {
	ID         string
	SiteID     string
	Name       string
	LastSeenMS int64 // 0 if never seen
	Sensors    []SensorInfo
}

// SensorInfo is one sensor attached to a machine.
type SensorInfo struct {
	ID         string
	Kind       string
	LastSeenMS int64
}

// Bucket is one downsampled point of a metric series.
type Bucket struct {
	T   int64   // bucket start, unix ms
	Avg float64 // mean value in the bucket
	Min float64 // minimum value in the bucket
	Max float64 // maximum value in the bucket
	N   int     // number of raw samples in the bucket
}

const minuteMS = 60_000

// migrations are applied in order; PRAGMA user_version records how many ran.
// Never edit an applied migration — append a new one.
var migrations = []string{
	// 1: initial generic model.
	`
CREATE TABLE sites (
  id   TEXT PRIMARY KEY,
  name TEXT NOT NULL
);
CREATE TABLE machines (
  id           TEXT PRIMARY KEY,
  site_id      TEXT NOT NULL REFERENCES sites(id),
  name         TEXT NOT NULL,
  last_seen_ms INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE sensors (
  machine_id   TEXT NOT NULL REFERENCES machines(id),
  id           TEXT NOT NULL,
  kind         TEXT NOT NULL DEFAULT '',
  last_seen_ms INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (machine_id, id)
);

-- Raw values, pruned after RAW_RETENTION.
CREATE TABLE measurements (
  machine_id  TEXT    NOT NULL,
  sensor_id   TEXT    NOT NULL,
  metric      TEXT    NOT NULL,
  value       REAL    NOT NULL,
  ts          INTEGER NOT NULL,
  received_at INTEGER NOT NULL
);
CREATE INDEX idx_measurements_machine_metric_ts ON measurements(machine_id, metric, ts);
CREATE INDEX idx_measurements_received ON measurements(received_at);

-- Per-minute rollups, maintained on insert and kept indefinitely.
CREATE TABLE measurements_1m (
  machine_id TEXT    NOT NULL,
  sensor_id  TEXT    NOT NULL,
  metric     TEXT    NOT NULL,
  bucket     INTEGER NOT NULL,
  n          INTEGER NOT NULL,
  sum        REAL    NOT NULL,
  min        REAL    NOT NULL,
  max        REAL    NOT NULL,
  PRIMARY KEY (machine_id, metric, sensor_id, bucket)
) WITHOUT ROWID;

-- One row per state interval; end_ts is NULL for the current state.
CREATE TABLE state_events (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  machine_id TEXT    NOT NULL REFERENCES machines(id),
  state      TEXT    NOT NULL,
  start_ts   INTEGER NOT NULL,
  end_ts     INTEGER,
  reason     TEXT    NOT NULL DEFAULT ''
);
CREATE INDEX idx_state_events_machine_start ON state_events(machine_id, start_ts);

-- One row per job; status is running | completed | aborted.
CREATE TABLE jobs (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  machine_id TEXT    NOT NULL REFERENCES machines(id),
  start_ts   INTEGER NOT NULL,
  end_ts     INTEGER,
  energy_wh  REAL,
  status     TEXT    NOT NULL DEFAULT 'running'
);
CREATE INDEX idx_jobs_machine_start ON jobs(machine_id, start_ts);
`,
}

// Open opens (creating if needed) the SQLite database at path, enables WAL mode,
// and applies pending migrations.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// A single writer avoids "database is locked" under concurrent ingest +
	// HTTP reads; WAL still allows concurrent readers.
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL;",
		"PRAGMA busy_timeout=5000;",
		"PRAGMA synchronous=NORMAL;",
		"PRAGMA foreign_keys=ON;",
		"PRAGMA temp_store=MEMORY;", // avoid needing a temp dir (scratch image has none)
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("pragma %q: %w", pragma, err)
		}
	}

	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for i := version; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("migrate %d: %w", i+1, err)
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate %d: %w", i+1, err)
		}
		// PRAGMA does not accept bind parameters.
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate %d: set version: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migrate %d: commit: %w", i+1, err)
		}
	}
	return nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

// SyncRegistry upserts the sites, machines and sensors from the machines file,
// so configured names win over auto-registered placeholders. Entries that are
// in the database but not in the file are left alone (history is kept).
func (s *Store) SyncRegistry(ctx context.Context, m config.Machines) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sync registry: %w", err)
	}
	defer tx.Rollback()
	for _, site := range m.Sites {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO sites (id, name) VALUES (?, ?)
			 ON CONFLICT(id) DO UPDATE SET name = excluded.name`,
			site.ID, nameOr(site.Name, site.ID)); err != nil {
			return fmt.Errorf("sync site %s: %w", site.ID, err)
		}
		for _, mc := range site.Machines {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO machines (id, site_id, name) VALUES (?, ?, ?)
				 ON CONFLICT(id) DO UPDATE SET site_id = excluded.site_id, name = excluded.name`,
				mc.ID, site.ID, nameOr(mc.Name, mc.ID)); err != nil {
				return fmt.Errorf("sync machine %s: %w", mc.ID, err)
			}
			for _, sn := range mc.Sensors {
				if _, err := tx.ExecContext(ctx,
					`INSERT INTO sensors (machine_id, id, kind) VALUES (?, ?, ?)
					 ON CONFLICT(machine_id, id) DO UPDATE SET kind = excluded.kind`,
					mc.ID, sn.ID, sn.Kind); err != nil {
					return fmt.Errorf("sync sensor %s/%s: %w", mc.ID, sn.ID, err)
				}
			}
		}
	}
	return tx.Commit()
}

// MachineExists reports whether a machine is registered.
func (s *Store) MachineExists(ctx context.Context, machineID string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx, `SELECT 1 FROM machines WHERE id = ?`, machineID).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("machine exists: %w", err)
	}
	return true, nil
}

// InsertMeasurements atomically stores one message's worth of measurements:
// raw rows, the per-minute rollups, and liveness. Unknown sites/machines/sensors
// are registered on the fly (callers decide whether to allow that).
func (s *Store) InsertMeasurements(ctx context.Context, ms []Measurement) error {
	if len(ms) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("insert measurements: %w", err)
	}
	defer tx.Rollback()

	registered := map[string]bool{}
	for _, m := range ms {
		key := m.MachineID + "/" + m.SensorID
		if !registered[key] {
			registered[key] = true
			if err := ensureRegistered(ctx, tx, m); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO measurements (machine_id, sensor_id, metric, value, ts, received_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			m.MachineID, m.SensorID, m.Metric, m.Value, m.TS, m.ReceivedAt); err != nil {
			return fmt.Errorf("insert measurement: %w", err)
		}
		bucket := m.TS - floorMod(m.TS, minuteMS)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO measurements_1m (machine_id, sensor_id, metric, bucket, n, sum, min, max)
			 VALUES (?, ?, ?, ?, 1, ?, ?, ?)
			 ON CONFLICT(machine_id, metric, sensor_id, bucket) DO UPDATE SET
			   n   = n + 1,
			   sum = sum + excluded.sum,
			   min = MIN(min, excluded.min),
			   max = MAX(max, excluded.max)`,
			m.MachineID, m.SensorID, m.Metric, bucket, m.Value, m.Value, m.Value); err != nil {
			return fmt.Errorf("update rollup: %w", err)
		}
	}
	return tx.Commit()
}

func ensureRegistered(ctx context.Context, tx *sql.Tx, m Measurement) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO sites (id, name) VALUES (?, ?)`, m.SiteID, m.SiteID); err != nil {
		return fmt.Errorf("register site: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO machines (id, site_id, name) VALUES (?, ?, ?)`,
		m.MachineID, m.SiteID, m.MachineID); err != nil {
		return fmt.Errorf("register machine: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT OR IGNORE INTO sensors (machine_id, id) VALUES (?, ?)`,
		m.MachineID, m.SensorID); err != nil {
		return fmt.Errorf("register sensor: %w", err)
	}
	// Liveness is arrival time, not series time: a backfill must not make a
	// dead sensor look alive, and a skewed clock must not make a live one stale.
	if _, err := tx.ExecContext(ctx,
		`UPDATE machines SET last_seen_ms = MAX(last_seen_ms, ?) WHERE id = ?`,
		m.ReceivedAt, m.MachineID); err != nil {
		return fmt.Errorf("touch machine: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE sensors SET last_seen_ms = MAX(last_seen_ms, ?) WHERE machine_id = ? AND id = ?`,
		m.ReceivedAt, m.MachineID, m.SensorID); err != nil {
		return fmt.Errorf("touch sensor: %w", err)
	}
	return nil
}

// PruneRaw deletes raw measurements received before cutoffMS. Rollups stay.
func (s *Store) PruneRaw(ctx context.Context, cutoffMS int64) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM measurements WHERE received_at < ?`, cutoffMS)
	if err != nil {
		return 0, fmt.Errorf("prune raw: %w", err)
	}
	return res.RowsAffected()
}

// Machines returns all machines with their sensors, ordered by site then id.
func (s *Store) Machines(ctx context.Context) ([]MachineInfo, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, site_id, name, last_seen_ms FROM machines ORDER BY site_id, id`)
	if err != nil {
		return nil, fmt.Errorf("machines query: %w", err)
	}
	var out []MachineInfo
	idx := map[string]int{}
	for rows.Next() {
		var m MachineInfo
		if err := rows.Scan(&m.ID, &m.SiteID, &m.Name, &m.LastSeenMS); err != nil {
			rows.Close()
			return nil, fmt.Errorf("machines scan: %w", err)
		}
		idx[m.ID] = len(out)
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("machines rows: %w", err)
	}

	srows, err := s.db.QueryContext(ctx,
		`SELECT machine_id, id, kind, last_seen_ms FROM sensors ORDER BY machine_id, id`)
	if err != nil {
		return nil, fmt.Errorf("sensors query: %w", err)
	}
	defer srows.Close()
	for srows.Next() {
		var machineID string
		var sn SensorInfo
		if err := srows.Scan(&machineID, &sn.ID, &sn.Kind, &sn.LastSeenMS); err != nil {
			return nil, fmt.Errorf("sensors scan: %w", err)
		}
		if i, ok := idx[machineID]; ok {
			out[i].Sensors = append(out[i].Sensors, sn)
		}
	}
	if err := srows.Err(); err != nil {
		return nil, fmt.Errorf("sensors rows: %w", err)
	}
	return out, nil
}

// Series returns a downsampled series of one metric on one machine over
// [fromMS, toMS), averaged across that machine's sensors reporting the metric.
// Buckets of a minute or more come from the rollups (available beyond raw
// retention); finer buckets come from raw rows.
func (s *Store) Series(ctx context.Context, machineID, metric string, fromMS, toMS, bucketMS int64) ([]Bucket, error) {
	if bucketMS <= 0 {
		bucketMS = minuteMS
	}
	var q string
	var args []any
	if bucketMS >= minuteMS && bucketMS%minuteMS == 0 {
		q = `SELECT (bucket / ?) * ? AS b, SUM(sum) / SUM(n), MIN(min), MAX(max), SUM(n)
		     FROM measurements_1m
		     WHERE machine_id = ? AND metric = ? AND bucket >= ? AND bucket < ?
		     GROUP BY b ORDER BY b ASC`
		// Align the rollup range to whole minutes so a partial first minute isn't lost.
		args = []any{bucketMS, bucketMS, machineID, metric, fromMS - floorMod(fromMS, minuteMS), toMS}
	} else {
		q = `SELECT (ts / ?) * ? AS b, AVG(value), MIN(value), MAX(value), COUNT(*)
		     FROM measurements
		     WHERE machine_id = ? AND metric = ? AND ts >= ? AND ts < ?
		     GROUP BY b ORDER BY b ASC`
		args = []any{bucketMS, bucketMS, machineID, metric, fromMS, toMS}
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("series query: %w", err)
	}
	defer rows.Close()

	var out []Bucket
	for rows.Next() {
		var b Bucket
		if err := rows.Scan(&b.T, &b.Avg, &b.Min, &b.Max, &b.N); err != nil {
			return nil, fmt.Errorf("series scan: %w", err)
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("series rows: %w", err)
	}
	return out, nil
}

// Metrics returns the distinct metric names recorded for a machine.
func (s *Store) Metrics(ctx context.Context, machineID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT metric FROM measurements_1m WHERE machine_id = ? ORDER BY metric`, machineID)
	if err != nil {
		return nil, fmt.Errorf("metrics query: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("metrics scan: %w", err)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// floorMod is a modulo that is non-negative for negative a (pre-1970 is
// nonsense here, but bucketing must never panic or misplace on it).
func floorMod(a, b int64) int64 {
	m := a % b
	if m < 0 {
		m += b
	}
	return m
}

func nameOr(name, fallback string) string {
	if name == "" {
		return fallback
	}
	return name
}

// Sample is one raw value of a metric.
type Sample struct {
	TS    int64
	Value float64
}

// RecentSamples returns the raw values of one metric on a machine with
// ts > sinceMS, oldest first. An empty sensorID matches every sensor.
func (s *Store) RecentSamples(ctx context.Context, machineID, sensorID, metric string, sinceMS int64) ([]Sample, error) {
	q := `SELECT ts, value FROM measurements WHERE machine_id = ? AND metric = ? AND ts > ?`
	args := []any{machineID, metric, sinceMS}
	if sensorID != "" {
		q += ` AND sensor_id = ?`
		args = append(args, sensorID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY ts ASC`, args...)
	if err != nil {
		return nil, fmt.Errorf("recent samples: %w", err)
	}
	defer rows.Close()
	var out []Sample
	for rows.Next() {
		var sm Sample
		if err := rows.Scan(&sm.TS, &sm.Value); err != nil {
			return nil, fmt.Errorf("recent samples scan: %w", err)
		}
		out = append(out, sm)
	}
	return out, rows.Err()
}

// StateEvent is one state interval; End is 0 while it is the current state.
type StateEvent struct {
	State  string
	Start  int64
	End    int64
	Reason string
}

// CurrentState returns a machine's open state event; ok is false if it has
// none yet.
func (s *Store) CurrentState(ctx context.Context, machineID string) (ev StateEvent, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT state, start_ts, reason FROM state_events
		 WHERE machine_id = ? AND end_ts IS NULL ORDER BY start_ts DESC, id DESC LIMIT 1`,
		machineID).Scan(&ev.State, &ev.Start, &ev.Reason)
	if err == sql.ErrNoRows {
		return ev, false, nil
	}
	if err != nil {
		return ev, false, fmt.Errorf("current state: %w", err)
	}
	return ev, true, nil
}

// RecordState closes the machine's open state event at startMS and opens a
// new one, atomically.
func (s *Store) RecordState(ctx context.Context, machineID, state string, startMS int64, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record state: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE state_events SET end_ts = MAX(start_ts, ?) WHERE machine_id = ? AND end_ts IS NULL`,
		startMS, machineID); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO state_events (machine_id, state, start_ts, reason) VALUES (?, ?, ?, ?)`,
		machineID, state, startMS, reason); err != nil {
		return fmt.Errorf("open state: %w", err)
	}
	return tx.Commit()
}

// StateEvents returns the state intervals of a machine overlapping
// [fromMS, toMS), oldest first. Intervals are not clipped to the range.
func (s *Store) StateEvents(ctx context.Context, machineID string, fromMS, toMS int64) ([]StateEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT state, start_ts, COALESCE(end_ts, 0), reason FROM state_events
		 WHERE machine_id = ? AND start_ts < ? AND (end_ts IS NULL OR end_ts > ?)
		 ORDER BY start_ts ASC, id ASC`,
		machineID, toMS, fromMS)
	if err != nil {
		return nil, fmt.Errorf("state events: %w", err)
	}
	defer rows.Close()
	var out []StateEvent
	for rows.Next() {
		var ev StateEvent
		if err := rows.Scan(&ev.State, &ev.Start, &ev.End, &ev.Reason); err != nil {
			return nil, fmt.Errorf("state events scan: %w", err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
