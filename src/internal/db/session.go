package db

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"
)

type Session struct {
	Token     string
	UserID    int64
	ExpiresAt int64
}

// hashToken derives the stored form of a session token. The database never
// holds the raw token: a local reader of the DB file (backup, misconfigured
// permissions) must not be able to replay sessions. SHA-256 of a 256-bit
// random token is sufficient — the token has no structure to brute-force.
func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

func (d *DB) CreateSession(userID int64, days int) (*Session, error) {
	return d.createSession(userID, days, false)
}

// CreateImpersonatedSession creates a user session on behalf of the operator
// ("log in as user"). It is indistinguishable from a real login to the user
// panel except for the impersonated flag, which the panel uses to attribute
// audit events to the operator ("000+<user>") and to show a "logged in as"
// banner.
func (d *DB) CreateImpersonatedSession(userID int64, days int) (*Session, error) {
	return d.createSession(userID, days, true)
}

func (d *DB) createSession(userID int64, days int, impersonated bool) (*Session, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	// The raw token is returned to the caller (it goes into the cookie); only
	// its hash is stored, so a DB leak does not expose usable sessions.
	token := hex.EncodeToString(b)
	s := &Session{Token: token, UserID: userID,
		ExpiresAt: time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix()}
	_, err := d.sql.Exec(`INSERT INTO sessions(token, user_id, expires_at, impersonated) VALUES(?,?,?,?)`,
		hashToken(token), s.UserID, s.ExpiresAt, b2i(impersonated))
	if err != nil {
		return nil, err
	}
	return s, nil
}

// SessionUser returns the user for a valid (unexpired) session token.
func (d *DB) SessionUser(token string) (*User, error) {
	u, _, err := d.SessionWithFlag(token)
	return u, err
}

// SessionWithFlag returns the user for a valid session token plus whether the
// session was created by the operator (impersonation).
func (d *DB) SessionWithFlag(token string) (*User, bool, error) {
	var userID int64
	var expiresAt int64
	var imp int
	err := d.sql.QueryRow(`SELECT user_id, expires_at, impersonated FROM sessions WHERE token=?`, hashToken(token)).
		Scan(&userID, &expiresAt, &imp)
	if err != nil {
		return nil, false, err
	}
	if time.Now().Unix() > expiresAt {
		d.DeleteSession(token)
		return nil, false, errors.New("session expired")
	}
	u, err := d.GetUserByID(userID)
	if err != nil {
		return nil, false, err
	}
	return u, imp != 0, nil
}

func (d *DB) DeleteSession(token string) error {
	_, err := d.sql.Exec(`DELETE FROM sessions WHERE token=?`, hashToken(token))
	return err
}

func (d *DB) DeleteSessionsForUser(userID int64) error {
	_, err := d.sql.Exec(`DELETE FROM sessions WHERE user_id=?`, userID)
	return err
}

// DeleteSessionsForUserExcept invalidates all sessions of a user except the
// one identified by keepToken. An empty keepToken deletes every session.
func (d *DB) DeleteSessionsForUserExcept(userID int64, keepToken string) error {
	if keepToken == "" {
		return d.DeleteSessionsForUser(userID)
	}
	_, err := d.sql.Exec(`DELETE FROM sessions WHERE user_id=? AND token<>?`, userID, hashToken(keepToken))
	return err
}
