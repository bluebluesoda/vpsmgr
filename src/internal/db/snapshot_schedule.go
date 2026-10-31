package db

import (
	"database/sql"
	"errors"
	"fmt"
)

// SnapshotSchedule is a user's opt-in automatic snapshot policy and execution
// history. Times are Unix seconds; zero history timestamps mean no attempt yet.
type SnapshotSchedule struct {
	UserID        int64
	Enabled       bool
	IntervalHours int
	NextRunAt     int64
	LastRunAt     int64
	LastSuccessAt int64
	LastError     string
}

const snapshotScheduleColumns = `user_id, enabled, interval_hours, next_run_at, last_run_at, last_success_at, last_error`

func scanSnapshotSchedule(row interface{ Scan(...any) error }) (*SnapshotSchedule, error) {
	var s SnapshotSchedule
	if err := row.Scan(&s.UserID, &s.Enabled, &s.IntervalHours, &s.NextRunAt,
		&s.LastRunAt, &s.LastSuccessAt, &s.LastError); err != nil {
		return nil, err
	}
	return &s, nil
}

// GetSnapshotSchedule returns nil, nil when the user has no policy.
func (d *DB) GetSnapshotSchedule(userID int64) (*SnapshotSchedule, error) {
	s, err := scanSnapshotSchedule(d.sql.QueryRow(
		`SELECT `+snapshotScheduleColumns+` FROM snapshot_schedules WHERE user_id=?`, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return s, err
}

// ListDueSnapshotSchedules orders due policies by next run, then user ID.
func (d *DB) ListDueSnapshotSchedules(now int64) ([]SnapshotSchedule, error) {
	rows, err := d.sql.Query(`SELECT `+snapshotScheduleColumns+`
		FROM snapshot_schedules WHERE enabled=1 AND next_run_at<=?
		ORDER BY next_run_at, user_id`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SnapshotSchedule
	for rows.Next() {
		s, err := scanSnapshotSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

// SaveSnapshotSchedule changes policy without changing execution history.
// Disabling also converts every automatic snapshot to fixed in the same short
// transaction; it does not delete any actual container snapshots.
func (d *DB) SaveSnapshotSchedule(userID int64, enabled bool, hours int, next int64) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO snapshot_schedules(user_id, enabled, interval_hours, next_run_at)
		VALUES(?,?,?,?) ON CONFLICT(user_id) DO UPDATE SET
		enabled=excluded.enabled, interval_hours=excluded.interval_hours, next_run_at=excluded.next_run_at`,
		userID, enabled, hours, next); err != nil {
		return err
	}
	if !enabled {
		if _, err := tx.Exec(`DELETE FROM automatic_snapshots WHERE user_id=?`, userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// BeginAutomaticSnapshot records the intent BEFORE Incus creates the snapshot,
// so a crash cannot leave an automatic snapshot misclassified as fixed. The
// caller must serialize container operations with LockOperations.
func (d *DB) BeginAutomaticSnapshot(userID int64, snapshot string, runAt, next int64) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE snapshot_schedules SET last_run_at=?, next_run_at=?, last_error=''
		WHERE user_id=? AND enabled=1`, runAt, next, userID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("snapshot schedule for user %d is missing or disabled", userID)
	}
	if _, err := tx.Exec(`INSERT INTO automatic_snapshots(user_id, snapshot) VALUES(?,?)`, userID, snapshot); err != nil {
		return err
	}
	return tx.Commit()
}

// FinishAutomaticSnapshot records the result without shifting the next run.
// A failed attempt (successAt <= 0) preserves the last successful timestamp.
func (d *DB) FinishAutomaticSnapshot(userID int64, successAt int64, result string) error {
	_, err := d.sql.Exec(`UPDATE snapshot_schedules SET last_error=?,
		last_success_at=CASE WHEN ?>0 THEN ? ELSE last_success_at END WHERE user_id=?`,
		result, successAt, successAt, userID)
	return err
}

// RecordSnapshotAttempt advances a skipped or failed attempt with no new point.
func (d *DB) RecordSnapshotAttempt(userID int64, runAt, next int64, result string) error {
	_, err := d.sql.Exec(`UPDATE snapshot_schedules SET last_run_at=?, next_run_at=?, last_error=? WHERE user_id=?`,
		runAt, next, result, userID)
	return err
}

func (d *DB) AutomaticSnapshots(userID int64) (map[string]bool, error) {
	rows, err := d.sql.Query(`SELECT snapshot FROM automatic_snapshots WHERE user_id=?`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// DeleteAutomaticSnapshot removes metadata only (it does not call Incus).
func (d *DB) DeleteAutomaticSnapshot(userID int64, name string) error {
	_, err := d.sql.Exec(`DELETE FROM automatic_snapshots WHERE user_id=? AND snapshot=?`, userID, name)
	return err
}

func (d *DB) ClearAutomaticSnapshots(userID int64) error {
	_, err := d.sql.Exec(`DELETE FROM automatic_snapshots WHERE user_id=?`, userID)
	return err
}

// ResetSnapshotSchedule clears old container metadata and postpones an existing
// enabled policy. Disabled policies, execution history and intervals are kept.
func (d *DB) ResetSnapshotSchedule(userID int64, next int64) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM automatic_snapshots WHERE user_id=?`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE snapshot_schedules SET next_run_at=? WHERE user_id=? AND enabled=1`, next, userID); err != nil {
		return err
	}
	return tx.Commit()
}
