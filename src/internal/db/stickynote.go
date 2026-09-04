package db

import (
	"database/sql"
	"errors"
)

// sticky_notes holds one opaque encrypted blob per user; the server never
// sees the plaintext (the panel encrypts with a browser-only key). An empty
// row means the notes were never enabled or were reset.

// GetStickyNotes returns the stored encrypted envelope for a user, or "" when
// no notes exist yet (never enabled or reset).
func (d *DB) GetStickyNotes(userID int64) (string, error) {
	var data string
	err := d.sql.QueryRow(`SELECT data FROM sticky_notes WHERE user_id=?`, userID).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return data, err
}

// SetStickyNotes stores (or replaces) a user's encrypted envelope. Passing ""
// clears the row back to the un-enabled state.
func (d *DB) SetStickyNotes(userID int64, data string) error {
	_, err := d.sql.Exec(`INSERT INTO sticky_notes(user_id, data) VALUES(?, ?)
		ON CONFLICT(user_id) DO UPDATE SET data=excluded.data`, userID, data)
	return err
}
