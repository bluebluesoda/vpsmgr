package mgr

import (
	"errors"

	"vpsmgr/internal/lx"
)

// AttachTerminal opens an interactive shell inside the named container. The
// caller owns the returned session and must Close it when done.
func (m *Manager) AttachTerminal(name string, size lx.TermSize) (*lx.Pty, error) {
	// A stopped container fails deep inside the exec call with a message that
	// means nothing to a user, so say something useful instead.
	if st, err := m.lx.InstanceStatuses(); err == nil {
		if status, ok := st[name]; ok && status != "Running" {
			return nil, errors.New("container is not running")
		}
	}
	return m.lx.Attach(name, size)
}
