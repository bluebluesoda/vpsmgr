package db

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"
)

type Session struct {
	Token     string
	UserID    int64
	ExpiresAt int64
}

func (d *DB) CreateSession(userID int64, days int) (*Session, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	s := &Session{Token: hex.EncodeToString(b), UserID: userID,
		ExpiresAt: time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix()}
	_, err := d.sql.Exec(`INSERT INTO sessions(token, user_id, expires_at) VALUES(?,?,?)`,
		s.Token, s.UserID, s.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// SessionUser returns the user for a valid (unexpired) session token.
func (d *DB) SessionUser(token string) (*User, error) {
	var userID int64
	var expiresAt int64
	err := d.sql.QueryRow(`SELECT user_id, expires_at FROM sessions WHERE token=?`, token).
		Scan(&userID, &expiresAt)
	if err != nil {
		return nil, err
	}
	if time.Now().Unix() > expiresAt {
		d.DeleteSession(token)
		return nil, errors.New("session expired")
	}
	return d.GetUserByID(userID)
}

func (d *DB) DeleteSession(token string) error {
	_, err := d.sql.Exec(`DELETE FROM sessions WHERE token=?`, token)
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
	_, err := d.sql.Exec(`DELETE FROM sessions WHERE user_id=? AND token<>?`, userID, keepToken)
	return err
}
