package db

import (
	"database/sql"
	"errors"
)

// SnapshotShare is one published checkpoint: a random code that anyone can
// present to clone the owner's snapshot into a fresh container. A user has at
// most one share at a time (see the UNIQUE(user_id) index).
type SnapshotShare struct {
	Code      string
	UserID    int64
	Snapshot  string
	CreatedAt string
}

// SetSnapshotShare publishes a share for the user, replacing any existing one.
func (d *DB) SetSnapshotShare(userID int64, code, snapshot string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM snapshot_shares WHERE user_id=?`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO snapshot_shares(code, user_id, snapshot, created_at) VALUES(?,?,?,?)`,
		code, userID, snapshot, now()); err != nil {
		return err
	}
	return tx.Commit()
}

// GetSnapshotShare returns the user's current share, or (nil, nil) when none.
func (d *DB) GetSnapshotShare(userID int64) (*SnapshotShare, error) {
	var s SnapshotShare
	switch err := d.sql.QueryRow(
		`SELECT code, user_id, snapshot, created_at FROM snapshot_shares WHERE user_id=?`, userID).
		Scan(&s.Code, &s.UserID, &s.Snapshot, &s.CreatedAt); {
	case err == nil:
		return &s, nil
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	default:
		return nil, err
	}
}

// GetSnapshotShareByCode resolves a share code, or (nil, nil) when unknown.
func (d *DB) GetSnapshotShareByCode(code string) (*SnapshotShare, error) {
	var s SnapshotShare
	switch err := d.sql.QueryRow(
		`SELECT code, user_id, snapshot, created_at FROM snapshot_shares WHERE code=?`, code).
		Scan(&s.Code, &s.UserID, &s.Snapshot, &s.CreatedAt); {
	case err == nil:
		return &s, nil
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	default:
		return nil, err
	}
}

// DeleteSnapshotShare removes the user's share (no-op when there is none).
func (d *DB) DeleteSnapshotShare(userID int64) error {
	_, err := d.sql.Exec(`DELETE FROM snapshot_shares WHERE user_id=?`, userID)
	return err
}

// DeleteSnapshotShareBySnapshot removes the user's share only when it points at
// the given snapshot, so deleting an unrelated checkpoint does not revoke it.
func (d *DB) DeleteSnapshotShareBySnapshot(userID int64, snapshot string) error {
	_, err := d.sql.Exec(`DELETE FROM snapshot_shares WHERE user_id=? AND snapshot=?`, userID, snapshot)
	return err
}
