package store

import (
	"context"
	"database/sql"
	"fmt"
)

// StateEvent is one state interval; End is 0 while it is the current state.
type StateEvent struct {
	State  string
	Start  int64
	End    int64
	Reason string
}

// RuleSet is one state config a machine has run with. Config is the JSON of
// the rules ("" for events detected before rule sets were tracked);
// ArchivedAt is 0 for the machine's current rule set.
type RuleSet struct {
	ID         int64
	Config     string
	CreatedAt  int64
	ArchivedAt int64
}

// currentSet is the SQL for the machine's current rule set id (0 if it has
// none yet); it takes the machine id as its one argument.
const currentSet = `COALESCE((SELECT id FROM state_rule_sets WHERE machine_id = ? AND archived_at IS NULL), 0)`

// CurrentState returns a machine's open state event under its current rule
// set; ok is false if it has none yet.
func (s *Store) CurrentState(ctx context.Context, machineID string) (ev StateEvent, ok bool, err error) {
	err = s.db.QueryRowContext(ctx,
		`SELECT state, start_ts, reason FROM state_events
		 WHERE machine_id = ? AND end_ts IS NULL AND rule_set_id = `+currentSet+`
		 ORDER BY start_ts DESC, id DESC LIMIT 1`,
		machineID, machineID).Scan(&ev.State, &ev.Start, &ev.Reason)
	if err == sql.ErrNoRows {
		return ev, false, nil
	}
	if err != nil {
		return ev, false, fmt.Errorf("current state: %w", err)
	}
	return ev, true, nil
}

// RecordState closes the machine's open state event at startMS and opens a
// new one under the current rule set, atomically.
func (s *Store) RecordState(ctx context.Context, machineID, state string, startMS int64, reason string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("record state: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE state_events SET end_ts = MAX(start_ts, ?)
		 WHERE machine_id = ? AND end_ts IS NULL AND rule_set_id = `+currentSet,
		startMS, machineID, machineID); err != nil {
		return fmt.Errorf("close state: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO state_events (machine_id, state, start_ts, reason, rule_set_id)
		 VALUES (?, ?, ?, ?, `+currentSet+`)`,
		machineID, state, startMS, reason, machineID); err != nil {
		return fmt.Errorf("open state: %w", err)
	}
	return tx.Commit()
}

// StateEvents returns the state intervals of a machine overlapping
// [fromMS, toMS), oldest first, under rule set ruleSetID (<= 0: the current
// one). Intervals are not clipped to the range.
func (s *Store) StateEvents(ctx context.Context, machineID string, ruleSetID, fromMS, toMS int64) ([]StateEvent, error) {
	set, args := "?", []any{machineID, ruleSetID, toMS, fromMS}
	if ruleSetID <= 0 {
		set, args = currentSet, []any{machineID, machineID, toMS, fromMS}
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT state, start_ts, COALESCE(end_ts, 0), reason FROM state_events
		 WHERE machine_id = ? AND rule_set_id = `+set+` AND start_ts < ? AND (end_ts IS NULL OR end_ts > ?)
		 ORDER BY start_ts ASC, id ASC`, args...)
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

// RuleSets lists a machine's rule sets, newest first.
func (s *Store) RuleSets(ctx context.Context, machineID string) ([]RuleSet, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, config, created_at, COALESCE(archived_at, 0) FROM state_rule_sets
		 WHERE machine_id = ? ORDER BY id DESC`, machineID)
	if err != nil {
		return nil, fmt.Errorf("rule sets: %w", err)
	}
	defer rows.Close()
	var out []RuleSet
	for rows.Next() {
		var r RuleSet
		if err := rows.Scan(&r.ID, &r.Config, &r.CreatedAt, &r.ArchivedAt); err != nil {
			return nil, fmt.Errorf("rule sets scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ActivateRuleSet makes the config with this hash the machine's current rule
// set. If it already is, nothing changes (created is false). Otherwise the
// previous rule set (and any events from before rule sets were tracked) is
// archived with its events, open events are closed at nowMS, and a new empty
// rule set is created for the caller to fill by replaying history.
func (s *Store) ActivateRuleSet(ctx context.Context, machineID, hash, config string, nowMS int64) (id int64, created bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("activate rule set: %w", err)
	}
	defer tx.Rollback()

	var curID int64
	var curHash string
	err = tx.QueryRowContext(ctx,
		`SELECT id, hash FROM state_rule_sets WHERE machine_id = ? AND archived_at IS NULL`,
		machineID).Scan(&curID, &curHash)
	if err != nil && err != sql.ErrNoRows {
		return 0, false, fmt.Errorf("current rule set: %w", err)
	}
	if err == nil && curHash == hash {
		return curID, false, nil
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE state_events SET end_ts = MAX(start_ts, ?)
		 WHERE machine_id = ? AND end_ts IS NULL AND rule_set_id IN (0, ?)`,
		nowMS, machineID, curID); err != nil {
		return 0, false, fmt.Errorf("close states: %w", err)
	}

	// Events from before rule sets get a rule set of their own (no config).
	var legacyFrom sql.NullInt64
	if err := tx.QueryRowContext(ctx,
		`SELECT MIN(start_ts) FROM state_events WHERE machine_id = ? AND rule_set_id = 0`,
		machineID).Scan(&legacyFrom); err != nil {
		return 0, false, fmt.Errorf("legacy states: %w", err)
	}
	if legacyFrom.Valid {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO state_rule_sets (machine_id, hash, config, created_at, archived_at) VALUES (?, '', '', ?, ?)`,
			machineID, legacyFrom.Int64, nowMS)
		if err != nil {
			return 0, false, fmt.Errorf("legacy rule set: %w", err)
		}
		legacyID, _ := res.LastInsertId()
		if _, err := tx.ExecContext(ctx,
			`UPDATE state_events SET rule_set_id = ? WHERE machine_id = ? AND rule_set_id = 0`,
			legacyID, machineID); err != nil {
			return 0, false, fmt.Errorf("archive legacy states: %w", err)
		}
	}
	if curID != 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE state_rule_sets SET archived_at = ? WHERE id = ?`, nowMS, curID); err != nil {
			return 0, false, fmt.Errorf("archive rule set: %w", err)
		}
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO state_rule_sets (machine_id, hash, config, created_at) VALUES (?, ?, ?, ?)`,
		machineID, hash, config, nowMS)
	if err != nil {
		return 0, false, fmt.Errorf("new rule set: %w", err)
	}
	if id, err = res.LastInsertId(); err != nil {
		return 0, false, fmt.Errorf("new rule set id: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("activate rule set: %w", err)
	}
	return id, true, nil
}

// FirstSample returns the time of the oldest stored value of a metric on a
// machine (rollups are kept forever, so this is the start of its history);
// ok is false if there is none. An empty sensorID matches every sensor.
func (s *Store) FirstSample(ctx context.Context, machineID, sensorID, metric string) (ts int64, ok bool, err error) {
	q, args := `SELECT MIN(bucket) FROM measurements_1m WHERE machine_id = ? AND metric = ?`, []any{machineID, metric}
	if sensorID != "" {
		q, args = q+` AND sensor_id = ?`, append(args, sensorID)
	}
	var v sql.NullInt64
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&v); err != nil {
		return 0, false, fmt.Errorf("first sample: %w", err)
	}
	return v.Int64, v.Valid, nil
}

// ReplaceStateEvents replaces the events of a rule set with evs (sorted by
// Start); each ends where the next starts and the last one stays open.
func (s *Store) ReplaceStateEvents(ctx context.Context, machineID string, ruleSetID int64, evs []StateEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("replace states: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM state_events WHERE machine_id = ? AND rule_set_id = ?`, machineID, ruleSetID); err != nil {
		return fmt.Errorf("replace states: %w", err)
	}
	for i, ev := range evs {
		var end any // NULL for the last (open) event
		if i+1 < len(evs) {
			end = evs[i+1].Start
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO state_events (machine_id, state, start_ts, end_ts, reason, rule_set_id) VALUES (?, ?, ?, ?, ?, ?)`,
			machineID, ev.State, ev.Start, end, ev.Reason, ruleSetID); err != nil {
			return fmt.Errorf("replace states: %w", err)
		}
	}
	return tx.Commit()
}

// SignalHistory returns one metric's values on a machine from fromMS on,
// oldest first: per-minute rollup averages (at the middle of each minute)
// where raw data has been pruned, raw values after that. An empty sensorID
// matches every sensor.
func (s *Store) SignalHistory(ctx context.Context, machineID, sensorID, metric string, fromMS int64) ([]Sample, error) {
	sensorQ, args := "", []any{machineID, metric}
	if sensorID != "" {
		sensorQ, args = " AND sensor_id = ?", append(args, sensorID)
	}
	var firstRaw sql.NullInt64
	if err := s.db.QueryRowContext(ctx,
		`SELECT MIN(ts) FROM measurements WHERE machine_id = ? AND metric = ?`+sensorQ, args...).Scan(&firstRaw); err != nil {
		return nil, fmt.Errorf("signal history: %w", err)
	}
	var out []Sample
	if !firstRaw.Valid || firstRaw.Int64 > fromMS {
		end := int64(1<<62 - 1)
		if firstRaw.Valid {
			end = firstRaw.Int64 - floorMod(firstRaw.Int64, minuteMS) // whole minutes before raw data
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT bucket, SUM(sum) / SUM(n) FROM measurements_1m
			 WHERE machine_id = ? AND metric = ?`+sensorQ+` AND bucket >= ? AND bucket < ?
			 GROUP BY bucket ORDER BY bucket`,
			append(append([]any{}, args...), fromMS-floorMod(fromMS, minuteMS), end)...)
		if err != nil {
			return nil, fmt.Errorf("signal history rollups: %w", err)
		}
		for rows.Next() {
			var sm Sample
			if err := rows.Scan(&sm.TS, &sm.Value); err != nil {
				rows.Close()
				return nil, fmt.Errorf("signal history scan: %w", err)
			}
			sm.TS += minuteMS / 2
			out = append(out, sm)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("signal history rollups: %w", err)
		}
	}
	raw, err := s.RecentSamples(ctx, machineID, sensorID, metric, fromMS-1)
	if err != nil {
		return nil, err
	}
	return append(out, raw...), nil
}

// EnergyWh returns the energy a machine used in [fromMS, toMS): the change of
// its lifetime energy counter (metric energy_wh, as power meters report it)
// if it has one, else power_w integrated over the per-minute rollups. ok is
// false when there is no data for either.
func (s *Store) EnergyWh(ctx context.Context, machineID string, fromMS, toMS int64) (wh float64, ok bool, err error) {
	var first, last sql.NullFloat64
	if err := s.db.QueryRowContext(ctx,
		`SELECT (SELECT value FROM measurements WHERE machine_id = ?1 AND metric = 'energy_wh' AND ts >= ?2 AND ts < ?3 ORDER BY ts ASC LIMIT 1),
		        (SELECT value FROM measurements WHERE machine_id = ?1 AND metric = 'energy_wh' AND ts >= ?2 AND ts < ?3 ORDER BY ts DESC LIMIT 1)`,
		machineID, fromMS, toMS).Scan(&first, &last); err != nil {
		return 0, false, fmt.Errorf("energy: %w", err)
	}
	if !first.Valid {
		// Raw data pruned: the counter's minute rollups.
		if err := s.db.QueryRowContext(ctx,
			`SELECT MIN(min), MAX(max) FROM measurements_1m
			 WHERE machine_id = ? AND metric = 'energy_wh' AND bucket >= ? AND bucket < ?`,
			machineID, fromMS-floorMod(fromMS, minuteMS), toMS).Scan(&first, &last); err != nil {
			return 0, false, fmt.Errorf("energy rollups: %w", err)
		}
	}
	if first.Valid && last.Valid && last.Float64 >= first.Float64 {
		return last.Float64 - first.Float64, true, nil
	}
	var integral sql.NullFloat64
	if err := s.db.QueryRowContext(ctx,
		`SELECT SUM(sum / n) / 60.0 FROM measurements_1m
		 WHERE machine_id = ? AND metric = 'power_w' AND bucket >= ? AND bucket < ?`,
		machineID, fromMS-floorMod(fromMS, minuteMS), toMS).Scan(&integral); err != nil {
		return 0, false, fmt.Errorf("energy from power: %w", err)
	}
	return integral.Float64, integral.Valid, nil
}
