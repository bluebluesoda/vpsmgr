package db

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Admin sessions are persisted so a panel restart (upgrade, crash,
// `systemctl restart vps`) does not log the operator out. Like user sessions,
// only the SHA-256 of the token is stored, so a DB copy cannot be replayed as a
// live session.

// CreateAdminSession issues a new admin session token valid for days days.
func (d *DB) CreateAdminSession(days int) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	exp := time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix()
	if _, err := d.sql.Exec(`INSERT INTO admin_sessions(token, expires_at) VALUES(?,?)`,
		hashToken(token), exp); err != nil {
		return "", err
	}
	return token, nil
}

// AdminSessionValid reports whether token is an unexpired admin session. An
// expired row is deleted on the spot.
func (d *DB) AdminSessionValid(token string) (bool, error) {
	var exp int64
	err := d.sql.QueryRow(`SELECT expires_at FROM admin_sessions WHERE token=?`, hashToken(token)).Scan(&exp)
	if err != nil {
		return false, nil // unknown token: not an error, just not a session
	}
	if time.Now().Unix() >= exp {
		_ = d.DeleteAdminSession(token)
		return false, nil
	}
	return true, nil
}

// DeleteAdminSession drops one session (logout).
func (d *DB) DeleteAdminSession(token string) error {
	_, err := d.sql.Exec(`DELETE FROM admin_sessions WHERE token=?`, hashToken(token))
	return err
}

// ClearAdminSessionsExcept drops every admin session except keepToken (empty
// keepToken drops them all). Used after an admin password change: the session
// that made the change stays, every other one is invalidated immediately.
func (d *DB) ClearAdminSessionsExcept(keepToken string) error {
	if keepToken == "" {
		_, err := d.sql.Exec(`DELETE FROM admin_sessions`)
		return err
	}
	_, err := d.sql.Exec(`DELETE FROM admin_sessions WHERE token<>?`, hashToken(keepToken))
	return err
}

// PruneAdminSessions deletes expired rows and, when the store has grown past
// max, the oldest remaining ones — the persistent equivalent of the cap the
// in-memory store enforced (`max` logins at a time).
func (d *DB) PruneAdminSessions(max int) error {
	now := time.Now().Unix()
	if _, err := d.sql.Exec(`DELETE FROM admin_sessions WHERE expires_at <= ?`, now); err != nil {
		return err
	}
	if max <= 0 {
		return nil
	}
	var n int
	if err := d.sql.QueryRow(`SELECT COUNT(*) FROM admin_sessions`).Scan(&n); err != nil {
		return err
	}
	if n < max {
		return nil
	}
	// Keep the newest max-1 rows so this session still fits under the cap.
	// Ordered by rowid (insertion order), NOT by expires_at: sessions created
	// within the same second share an expiry, so that would order them
	// arbitrarily and prune the wrong ones.
	_, err := d.sql.Exec(`DELETE FROM admin_sessions WHERE rowid IN (
		SELECT rowid FROM admin_sessions ORDER BY rowid DESC LIMIT -1 OFFSET ?)`, max-1)
	return err
}
