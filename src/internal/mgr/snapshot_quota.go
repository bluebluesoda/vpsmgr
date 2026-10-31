package mgr

import (
	"errors"
	"fmt"

	"vpsmgr/internal/db"
)

// validateSnapshotLimitLocked prevents quota edits from deleting recovery points
// or consuming the position reserved by an enabled automatic schedule. Increases
// remain possible even when Incus is unavailable; reductions fail closed.
func (m *Manager) validateSnapshotLimitLocked(u *db.User, limit int) error {
	if limit < 0 {
		return errors.New("snapshot quota must be a non-negative integer")
	}
	if limit >= u.SnapshotLimit {
		return nil
	}
	snaps, err := m.snapshotListLocked(u)
	if err != nil {
		return fmt.Errorf("cannot lower snapshot quota: %w", err)
	}
	if limit < len(snaps) {
		return fmt.Errorf("snapshot quota cannot be below the %d existing checkpoints", len(snaps))
	}
	schedule, err := m.db.GetSnapshotSchedule(u.ID)
	if err != nil {
		return err
	}
	if schedule != nil && schedule.Enabled && limit < fixedSnapshotCount(snaps)+1 {
		return errors.New("snapshot quota must reserve one position for the enabled automatic schedule")
	}
	return nil
}

// UpdateSnapshotLimit changes only an account's quota, never its checkpoints,
// policy or deadline. The shared lock covers catalog validation and persistence.
func (m *Manager) UpdateSnapshotLimit(name string, limit int) error {
	unlock, err := m.lockOperations()
	if err != nil {
		return err
	}
	defer unlock()
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return err
	}
	if err := m.validateSnapshotLimitLocked(u, limit); err != nil {
		return err
	}
	return m.db.UpdateSnapshotLimit(u.ID, limit)
}
