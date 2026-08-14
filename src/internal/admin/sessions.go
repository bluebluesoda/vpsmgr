package admin

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// sessionStore holds admin sessions in memory. Admin sessions are deliberately
// not persisted to the DB (no schema change); a panel service restart drops
// them and the admin logs in again.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]time.Time // token -> expiry (unix seconds)
	max      int
}

func newSessionStore(max int) *sessionStore {
	return &sessionStore{sessions: make(map[string]time.Time), max: max}
}

// create issues a new session token valid for days days.
func (s *sessionStore) create(days int) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := hex.EncodeToString(b)
	exp := time.Now().Add(time.Duration(days) * 24 * time.Hour)
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.sessions) >= s.max {
		s.pruneLocked()
	}
	s.sessions[token] = exp
	return token, nil
}

// valid reports whether token is an active session; expired tokens are lazily
// deleted.
func (s *sessionStore) valid(token string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(s.sessions, token)
		return false
	}
	return true
}

func (s *sessionStore) delete(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, token)
}

// clearExcept drops every session except token. Used after an admin password
// change: the changing session stays, all others are invalidated immediately.
func (s *sessionStore) clearExcept(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for t := range s.sessions {
		if t != token {
			delete(s.sessions, t)
		}
	}
}

// pruneLocked removes expired sessions; caller holds s.mu.
func (s *sessionStore) pruneLocked() {
	now := time.Now()
	for t, exp := range s.sessions {
		if now.After(exp) {
			delete(s.sessions, t)
		}
	}
}
