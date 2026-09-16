package admin

import "vpsmgr/internal/db"

// sessionStore persists admin sessions in the DB, so a panel restart (upgrade,
// crash, `systemctl restart vps`) no longer logs the operator out — previously
// they lived in process memory and every restart forced a fresh login. Like user
// sessions, only the SHA-256 of the token is stored, so a copy of the database
// cannot be replayed as a live session. max caps how many sessions may exist at
// once: expired rows and, past the cap, the oldest ones are pruned on create.
type sessionStore struct {
	db  *db.DB
	max int
}

func newSessionStore(d *db.DB, max int) *sessionStore {
	return &sessionStore{db: d, max: max}
}

// create issues a new session token valid for days days.
func (s *sessionStore) create(days int) (string, error) {
	if err := s.db.PruneAdminSessions(s.max); err != nil {
		return "", err
	}
	return s.db.CreateAdminSession(days)
}

// valid reports whether token is an active session; expired tokens are deleted.
func (s *sessionStore) valid(token string) bool {
	ok, err := s.db.AdminSessionValid(token)
	return err == nil && ok
}

func (s *sessionStore) delete(token string) {
	_ = s.db.DeleteAdminSession(token)
}

// clearExcept drops every session except token. Used after an admin password
// change: the changing session stays, all others are invalidated immediately.
func (s *sessionStore) clearExcept(token string) {
	_ = s.db.ClearAdminSessionsExcept(token)
}
