package mgr

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"time"

	"vpsmgr/internal/db"
	"vpsmgr/internal/lx"
)

// MaxSnapshotIntervalHours prevents a positive integer from overflowing a Go
// duration. Intervals are elapsed hours, independent of time zones and DST.
const MaxSnapshotIntervalHours = int((1<<63 - 1) / int64(time.Hour))

func snapshotInterval(hours int) (time.Duration, error) {
	if hours < 1 || hours > MaxSnapshotIntervalHours {
		return 0, fmt.Errorf("snapshot interval must be an integer from 1 to %d hours", MaxSnapshotIntervalHours)
	}
	return time.Duration(hours) * time.Hour, nil
}

// lockOperations extends the existing process-local lifecycle lock to CLI
// processes sharing the database's operation-lock sidecar. No SQLite
// transaction spans an Incus call.
func (m *Manager) lockOperations() (func(), error) {
	m.opMu.Lock()
	unlock, err := m.db.LockOperations()
	if err != nil {
		m.opMu.Unlock()
		return nil, err
	}
	return func() {
		unlock()
		m.opMu.Unlock()
	}, nil
}

func snapshotUserActive(u *db.User, now time.Time) error {
	if u.Status != db.StatusReady {
		return fmt.Errorf("container is not ready (%s)", u.Status)
	}
	if IsExpired(u.ExpiresAt, now) {
		return errors.New("account has expired")
	}
	return nil
}

func (m *Manager) SnapshotSchedule(name string) (*db.SnapshotSchedule, error) {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return nil, err
	}
	return m.db.GetSnapshotSchedule(u.ID)
}

// SetSnapshotSchedule reserves one position for automatic checkpoints, without
// removing or reclassifying existing fixed checkpoints. Disabling atomically
// fixes all automatic checkpoints and does not need a working Incus connection.
func (m *Manager) SetSnapshotSchedule(name string, enabled bool, hours int) error {
	unlock, err := m.lockOperations()
	if err != nil {
		return err
	}
	defer unlock()
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return err
	}
	current, err := m.db.GetSnapshotSchedule(u.ID)
	if err != nil {
		return err
	}
	if !enabled {
		hours = 1
		if current != nil {
			hours = current.IntervalHours
		}
		return m.db.SaveSnapshotSchedule(u.ID, false, hours, 0)
	}
	interval, err := snapshotInterval(hours)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if err := snapshotUserActive(u, now); err != nil {
		return err
	}
	limit := u.SnapshotLimit
	if limit == 0 {
		return errors.New("snapshots are disabled")
	}
	snaps, err := m.snapshotListLocked(u)
	if err != nil {
		return err
	}
	if current == nil || !current.Enabled {
		if len(snaps) >= limit {
			return errors.New("at least one free snapshot position is required to enable automatic snapshots")
		}
	} else if len(snaps) > limit || fixedSnapshotCount(snaps) > limit-1 {
		return errors.New("snapshot quota exceeded; delete checkpoints before updating the schedule")
	}
	if current != nil && current.Enabled && current.IntervalHours == hours {
		return nil // Re-submitting an unchanged form must not postpone the task.
	}
	return m.db.SaveSnapshotSchedule(u.ID, true, hours, now.Add(interval).Unix())
}

func (m *Manager) snapshotListLocked(u *db.User) ([]lx.SnapshotInfo, error) {
	snaps, err := m.lx.SnapshotList(u.Name)
	if err != nil {
		return nil, err
	}
	automatic, err := m.db.AutomaticSnapshots(u.ID)
	if err != nil {
		return nil, err
	}
	for i := range snaps {
		snaps[i].Automatic = automatic[snaps[i].Name]
	}
	return snaps, nil
}

func fixedSnapshotCount(snaps []lx.SnapshotInfo) int {
	fixed := 0
	for _, s := range snaps {
		if !s.Automatic {
			fixed++
		}
	}
	return fixed
}

// SnapshotPromote changes only ownership of a quota position, never the disk
// checkpoint or its creation time. A full total quota does not prevent it.
func (m *Manager) SnapshotPromote(name, snapshot string) error {
	unlock, err := m.lockOperations()
	if err != nil {
		return err
	}
	defer unlock()
	if !ValidSnapName(snapshot) {
		return errors.New("invalid snapshot name")
	}
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return err
	}
	if err := snapshotUserActive(u, time.Now()); err != nil {
		return err
	}
	schedule, err := m.db.GetSnapshotSchedule(u.ID)
	if err != nil {
		return err
	}
	if schedule == nil || !schedule.Enabled {
		return errors.New("automatic snapshots are not enabled")
	}
	snaps, err := m.snapshotListLocked(u)
	if err != nil {
		return err
	}
	found := false
	for _, s := range snaps {
		if s.Name == snapshot {
			if !s.Automatic {
				return errors.New("checkpoint is already fixed")
			}
			found = true
		}
	}
	if !found {
		return errors.New("snapshot not found")
	}
	if fixedSnapshotCount(snaps) >= u.SnapshotLimit-1 {
		return errors.New("fixed snapshot limit reached; delete a fixed checkpoint or disable automatic snapshots")
	}
	return m.db.DeleteAutomaticSnapshot(u.ID, snapshot)
}

func (m *Manager) deleteSnapshotLocked(u *db.User, name string) error {
	if err := m.lx.SnapshotDelete(u.Name, name); err != nil {
		return err
	}
	return errors.Join(m.db.DeleteAutomaticSnapshot(u.ID, name), m.db.DeleteSnapshotShareBySnapshot(u.ID, name))
}

// reconcileSnapshotMetadataLocked is used after restore, including failed
// restores which may already have discarded later checkpoints. Missing entries
// are removed only after a successful authoritative Incus listing.
func (m *Manager) reconcileSnapshotMetadataLocked(u *db.User) error {
	snaps, err := m.lx.SnapshotList(u.Name)
	if err != nil {
		return err
	}
	present := make(map[string]bool, len(snaps))
	for _, s := range snaps {
		present[s.Name] = true
	}
	automatic, err := m.db.AutomaticSnapshots(u.ID)
	if err != nil {
		return err
	}
	for name := range automatic {
		if !present[name] {
			if err := m.db.DeleteAutomaticSnapshot(u.ID, name); err != nil {
				return err
			}
		}
	}
	share, err := m.db.GetSnapshotShare(u.ID)
	if err != nil {
		return err
	}
	if share != nil && !present[share.Snapshot] {
		return m.db.DeleteSnapshotShare(u.ID)
	}
	return nil
}

func (m *Manager) resetSnapshotScheduleLocked(u *db.User) error {
	schedule, err := m.db.GetSnapshotSchedule(u.ID)
	if err != nil {
		return err
	}
	next := int64(0)
	if schedule != nil && schedule.Enabled {
		interval, err := snapshotInterval(schedule.IntervalHours)
		if err != nil {
			return err
		}
		next = time.Now().Add(interval).Unix()
	}
	return m.db.ResetSnapshotSchedule(u.ID, next)
}

// trimAutomaticSnapshotsLocked never removes a fixed checkpoint. Callers hold
// the lifecycle lock; a failed deletion stops the pass, leaving a recoverable
// over-quota state instead of repeatedly growing it on subsequent attempts.
func (m *Manager) trimAutomaticSnapshotsLocked(u *db.User, snaps []lx.SnapshotInfo, limit int) error {
	if len(snaps) <= limit {
		return nil
	}
	var candidates []lx.SnapshotInfo
	for _, s := range snaps {
		if s.Automatic {
			if snapshotCreateTime(s).IsZero() {
				return fmt.Errorf("cannot rotate checkpoint %s with an invalid creation time", s.Name)
			}
			candidates = append(candidates, s)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := snapshotCreateTime(candidates[i]), snapshotCreateTime(candidates[j])
		if a.Equal(b) {
			return candidates[i].Name < candidates[j].Name
		}
		return a.Before(b)
	})
	needed := len(snaps) - limit
	if len(candidates) < needed {
		return errors.New("snapshot quota exceeded by fixed checkpoints")
	}
	for _, s := range candidates[:needed] {
		if err := m.deleteSnapshotLocked(u, s.Name); err != nil {
			return fmt.Errorf("automatic checkpoint rotation failed for %s: %w", s.Name, err)
		}
	}
	return nil
}

// RunScheduledSnapshots performs at most one attempt per due user. Advancing
// the persisted deadline before creating the disk checkpoint prevents restart
// catch-up storms. Failed and skipped attempts wait the same configured interval.
func (m *Manager) RunScheduledSnapshots(now time.Time) error {
	due, err := m.db.ListDueSnapshotSchedules(now.Unix())
	if err != nil {
		return err
	}
	var errs []error
	for _, schedule := range due {
		if err := m.runScheduledSnapshot(schedule.UserID, now); err != nil {
			errs = append(errs, fmt.Errorf("automatic snapshots user %d: %w", schedule.UserID, err))
		}
	}
	return errors.Join(errs...)
}

func (m *Manager) runScheduledSnapshot(userID int64, now time.Time) error {
	unlock, err := m.lockOperations()
	if err != nil {
		return err
	}
	defer unlock()
	schedule, err := m.db.GetSnapshotSchedule(userID)
	if err != nil || schedule == nil || !schedule.Enabled || schedule.NextRunAt > now.Unix() {
		return err
	}
	u, err := m.db.GetUserByID(userID)
	if err != nil {
		return err
	}
	interval, err := snapshotInterval(schedule.IntervalHours)
	if err != nil {
		return err
	}
	next := now.Add(interval).Unix()
	skip := func(reason error) error {
		return errors.Join(reason, m.db.RecordSnapshotAttempt(userID, now.Unix(), next, reason.Error()))
	}
	if err := snapshotUserActive(u, now); err != nil {
		return skip(err)
	}
	limit := u.SnapshotLimit
	if limit == 0 {
		return skip(errors.New("snapshots are disabled"))
	}
	snaps, err := m.snapshotListLocked(u)
	if err != nil {
		return skip(err)
	}
	if fixedSnapshotCount(snaps) > limit-1 {
		return skip(errors.New("fixed snapshot quota exceeded; automatic snapshots are paused"))
	}
	// Repair an earlier interrupted/failed rotation before permitting another
	// creation. Only the scheduler, not manual creation, may reclaim auto points.
	if err := m.trimAutomaticSnapshotsLocked(u, snaps, limit); err != nil {
		return skip(err)
	}
	usage, err := m.PoolUsage()
	if err != nil {
		return skip(err)
	}
	if usage >= 0.9 {
		return skip(errors.New("storage pool is at least 90% used; automatic snapshot skipped"))
	}
	snapshot := "auto-" + snapName()[len("snap-"):]
	if err := m.db.BeginAutomaticSnapshot(userID, snapshot, now.Unix(), next); err != nil {
		return err
	}
	// The intent is durable before this call. If Incus succeeds but the panel
	// dies, the checkpoint is still classified as automatic after restart.
	if err := m.lx.SnapshotCreate(u.Name, snapshot); err != nil {
		return errors.Join(err, m.db.FinishAutomaticSnapshot(userID, 0, "creation failed: "+err.Error()))
	}
	_ = m.db.AddAuditLog("scheduler:"+u.Name, "snapshot.auto.create."+snapshot)
	snaps, err = m.snapshotListLocked(u)
	if err == nil {
		err = m.trimAutomaticSnapshotsLocked(u, snaps, limit)
	}
	if err != nil {
		return errors.Join(err, m.db.FinishAutomaticSnapshot(userID, now.Unix(), "created; rotation failed: "+err.Error()))
	}
	return m.db.FinishAutomaticSnapshot(userID, now.Unix(), "")
}

// RunSnapshotScheduler is independent of resource sampling and processes one
// checkpoint at a time, including when both HTTP panel entrances are disabled.
func (m *Manager) RunSnapshotScheduler(ctx context.Context) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := m.RunScheduledSnapshots(time.Now().UTC()); err != nil {
			log.Printf("automatic snapshots: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}
