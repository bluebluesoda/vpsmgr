package mgr

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
	"vpsmgr/internal/lx"
)

// snapshotDaemon exercises the actual lx HTTP/Unix-socket transport, without
// requiring Incus or privileged storage operations in the ordinary test suite.
type snapshotDaemon struct {
	mu         sync.Mutex
	snaps      []lx.SnapshotInfo
	calls      []string
	clock      time.Time
	createFail bool
	deleteFail bool
	listFail   bool
	usage      int
	running    bool
	restoreErr string
	entered    chan struct{}
	release    chan struct{}
}

func (f *snapshotDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	respond := func(metadata any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "sync", "status_code": 200,
			"operation": "/1.0/operations/test", "metadata": metadata})
	}
	fail := func(message string) {
		_ = json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": message, "error_code": 500})
	}
	path := r.URL.Path
	switch {
	case path == "/1.0/storage-pools/vpsmgr/resources":
		respond(map[string]any{"space": map[string]int{"total": 100, "used": f.usage}})
	case path == "/1.0/operations/test/wait":
		respond(map[string]string{"status": "Success", "err": ""})
	case path == "/1.0/instances/alice/snapshots" && r.Method == http.MethodGet:
		if f.listFail {
			fail("list failed")
			return
		}
		respond(append([]lx.SnapshotInfo{}, f.snaps...))
	case path == "/1.0/instances/alice/snapshots" && r.Method == http.MethodPost:
		var req struct{ Name string }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			fail(err.Error())
			return
		}
		f.calls = append(f.calls, "create:"+req.Name)
		if f.entered != nil {
			close(f.entered)
			f.entered = nil
			<-f.release
		}
		if f.createFail {
			fail("create failed")
			return
		}
		f.clock = f.clock.Add(time.Second)
		f.snaps = append(f.snaps, lx.SnapshotInfo{Name: req.Name, CreatedAt: f.clock.Format(time.RFC3339Nano)})
		respond(nil)
	case strings.HasPrefix(path, "/1.0/instances/alice/snapshots/") && r.Method == http.MethodDelete:
		name := strings.TrimPrefix(path, "/1.0/instances/alice/snapshots/")
		f.calls = append(f.calls, "delete:"+name)
		if f.deleteFail {
			fail("delete failed")
			return
		}
		for i, s := range f.snaps {
			if s.Name == name {
				f.snaps = append(f.snaps[:i], f.snaps[i+1:]...)
				respond(nil)
				return
			}
		}
		fail("snapshot not found")
	case path == "/1.0/instances/alice" && r.Method == http.MethodGet:
		status := "Stopped"
		if f.running {
			status = "Running"
		}
		respond(map[string]string{"status": status})
	case path == "/1.0/instances/alice/state":
		var req struct{ Action string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.calls = append(f.calls, req.Action)
		f.running = req.Action == "start"
		respond(nil)
	case path == "/1.0/instances/alice" && r.Method == http.MethodPut:
		var req struct {
			Restore  string `json:"restore"`
			DiskOnly bool   `json:"disk_only"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.calls = append(f.calls, "restore:"+req.Restore)
		if !req.DiskOnly {
			fail("restore must be disk-only")
		} else if f.restoreErr != "" {
			fail(f.restoreErr)
		} else {
			respond(nil) // Like btrfs: the manager must discard later points.
		}
	default:
		respond(map[string]any{}) // Host overview probes are nonessential here.
	}
}

func newSnapshotManager(t *testing.T, limit int) (*Manager, *db.User, *snapshotDaemon) {
	t.Helper()
	dir, err := os.MkdirTemp("", "vpssnap-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	listener, err := net.Listen("unix", filepath.Join(dir, "incus.sock"))
	if err != nil {
		t.Fatal(err)
	}
	f := &snapshotDaemon{clock: time.Now().UTC(), usage: 10}
	srv := &http.Server{Handler: f}
	go func() { _ = srv.Serve(listener) }()
	t.Cleanup(func() { _ = srv.Close() })
	c := cfg.Default()
	c.Incus.Socket = filepath.Join(dir, "incus.sock")
	c.Incus.Pool = "vpsmgr"
	c.Snapshots.Share = true
	d, err := db.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	u, err := d.CreateUserFull("alice", "unused", "192.0.2.2", 1, 30001, 10000, 1, 64, 1, 0, "", "", 0, "", "", limit)
	if err != nil {
		t.Fatal(err)
	}
	return New(c, d), u, f
}

func seedCheckpoints(t *testing.T, m *Manager, u *db.User, f *snapshotDaemon, fixed, automatic int) {
	t.Helper()
	if err := m.db.SaveSnapshotSchedule(u.ID, true, 1, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < fixed+automatic; i++ {
		name := fmt.Sprintf("fixed-%d", i)
		if i >= fixed {
			name = fmt.Sprintf("auto-%d", i-fixed)
			if err := m.db.BeginAutomaticSnapshot(u.ID, name, 0, time.Now().Add(time.Hour).Unix()); err != nil {
				t.Fatal(err)
			}
		}
		f.clock = f.clock.Add(time.Second)
		f.snaps = append(f.snaps, lx.SnapshotInfo{Name: name, CreatedAt: f.clock.Format(time.RFC3339Nano)})
	}
}

func runSnapshotDue(t *testing.T, m *Manager, u *db.User) error {
	t.Helper()
	now := time.Now().UTC()
	schedule, err := m.db.GetSnapshotSchedule(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.db.SaveSnapshotSchedule(u.ID, true, schedule.IntervalHours, now.Unix()); err != nil {
		t.Fatal(err)
	}
	return m.RunScheduledSnapshots(now)
}

func TestSnapshotManualAndPromotionQuotas(t *testing.T) {
	for _, tc := range []struct {
		fixed, auto     int
		create, promote bool
	}{
		{1, 4, false, true},
		{4, 1, false, false},
		{3, 1, true, true},
		{4, 0, false, false},
		{0, 0, true, false},
	} {
		t.Run(fmt.Sprintf("fixed%d-auto%d", tc.fixed, tc.auto), func(t *testing.T) {
			m, u, f := newSnapshotManager(t, 5)
			seedCheckpoints(t, m, u, f, tc.fixed, tc.auto)
			before := append([]lx.SnapshotInfo(nil), f.snaps...)
			err := m.SnapshotCreate(u.Name)
			if (err == nil) != tc.create {
				t.Fatalf("manual create = %v, allowed=%v", err, tc.create)
			}
			for _, call := range f.calls {
				if strings.HasPrefix(call, "delete:") {
					t.Fatal("manual create silently deleted a checkpoint")
				}
			}
			if !tc.create && !reflect.DeepEqual(before, f.snaps) {
				t.Fatal("rejected manual create changed checkpoints")
			}
			// Test promotion against the original quota, not a newly created point.
			if tc.create {
				f.snaps = before
			}
			before = append([]lx.SnapshotInfo(nil), f.snaps...)
			err = m.SnapshotPromote(u.Name, "auto-0")
			if (err == nil) != tc.promote {
				t.Fatalf("promote = %v, allowed=%v", err, tc.promote)
			}
			if !reflect.DeepEqual(before, f.snaps) {
				t.Fatal("promotion changed the disk checkpoint or its timestamp")
			}
		})
	}
}

func TestSnapshotScheduleEnableDisable(t *testing.T) {
	m, u, f := newSnapshotManager(t, 5)
	for _, hours := range []int{0, -1, MaxSnapshotIntervalHours + 1} {
		if err := m.SetSnapshotSchedule(u.Name, true, hours); err == nil {
			t.Fatalf("accepted interval %d", hours)
		}
	}
	before := time.Now().Add(2 * time.Hour).Unix()
	if err := m.SetSnapshotSchedule(u.Name, true, 2); err != nil {
		t.Fatal(err)
	}
	schedule, _ := m.SnapshotSchedule(u.Name)
	if schedule.NextRunAt < before || schedule.IntervalHours != 2 || !schedule.Enabled || schedule.LastSuccessAt == 0 {
		t.Fatalf("bad schedule: %+v", schedule)
	}
	if len(f.snaps) != 1 || len(f.calls) != 1 || !strings.HasPrefix(f.calls[0], "create:auto-") {
		t.Fatalf("enabling did not immediately create one automatic point: %v", f.calls)
	}
	initial, err := m.SnapshotList(u.Name)
	if err != nil || len(initial) != 1 || !initial[0].Automatic {
		t.Fatalf("initial checkpoint is not automatic: %v %v", initial, err)
	}
	if err := m.RunScheduledSnapshots(time.Now()); err != nil || len(f.calls) != 1 {
		t.Fatalf("scheduler repeated the initial checkpoint before the interval: %v", err)
	}
	if err := m.SetSnapshotSchedule(u.Name, true, 2); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := m.SnapshotSchedule(u.Name)
	if schedule.NextRunAt != unchanged.NextRunAt || len(f.calls) != 1 {
		t.Fatal("unchanged settings postponed the task or created another checkpoint")
	}
	seedCheckpoints(t, m, u, f, 1, 3)
	if err := m.SetSnapshotSchedule(u.Name, true, 3); err != nil {
		t.Fatalf("editing an enabled full schedule: %v", err)
	}
	f.listFail = true // Disabling is safe even if Incus is unavailable.
	if err := m.SetSnapshotSchedule(u.Name, false, 0); err != nil {
		t.Fatal(err)
	}
	f.listFail = false
	snaps, err := m.SnapshotList(u.Name)
	if err != nil || fixedSnapshotCount(snaps) != 5 {
		t.Fatalf("disable did not fix all checkpoints: %v %v", snaps, err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("interval edit or disable modified disk checkpoints: %v", f.calls)
	}
	if err := m.SetSnapshotSchedule(u.Name, true, 1); err == nil {
		t.Fatal("enabled automatic snapshots without a free position")
	}
	if err := m.SnapshotDelete(u.Name, "fixed-0"); err != nil {
		t.Fatal(err)
	}
	if err := m.SetSnapshotSchedule(u.Name, true, 1); err != nil {
		t.Fatal(err)
	}
	snaps, err = m.SnapshotList(u.Name)
	if err != nil || len(snaps) != 5 || fixedSnapshotCount(snaps) != 4 {
		t.Fatalf("re-enabling did not immediately use the free position: %v %v", snaps, err)
	}
}

func TestSnapshotScheduleInitialFailure(t *testing.T) {
	for _, failure := range []string{"create", "pool-full"} {
		t.Run(failure, func(t *testing.T) {
			m, u, f := newSnapshotManager(t, 2)
			seedCheckpoints(t, m, u, f, 1, 0)
			if err := m.SetSnapshotSchedule(u.Name, false, 0); err != nil {
				t.Fatal(err)
			}
			before := append([]lx.SnapshotInfo(nil), f.snaps...)
			f.createFail = failure == "create"
			if failure == "pool-full" {
				f.usage = 90
			}
			err := m.SetSnapshotSchedule(u.Name, true, 2)
			if err == nil || !strings.Contains(err.Error(), "enabled, but the initial checkpoint failed") {
				t.Fatalf("initial failure was not explained: %v", err)
			}
			schedule, err := m.SnapshotSchedule(u.Name)
			if err != nil || schedule == nil || !schedule.Enabled || schedule.LastRunAt == 0 || schedule.LastSuccessAt != 0 || schedule.LastError == "" || schedule.NextRunAt != schedule.LastRunAt+7200 {
				t.Fatalf("initial failure was not persisted for retry: %+v %v", schedule, err)
			}
			if !reflect.DeepEqual(f.snaps, before) {
				t.Fatal("initial failure changed an existing recovery point")
			}
			calls := len(f.calls)
			if err := m.RunScheduledSnapshots(time.Now()); err != nil || len(f.calls) != calls {
				t.Fatalf("initial failure retried before the interval: %v", err)
			}
			f.createFail, f.usage = false, 10
			if err := runSnapshotDue(t, m, u); err != nil {
				t.Fatal(err)
			}
			schedule, _ = m.SnapshotSchedule(u.Name)
			if len(f.snaps) != 2 || schedule.LastSuccessAt == 0 || schedule.LastError != "" {
				t.Fatal("scheduled retry did not recover the first automatic checkpoint")
			}
		})
	}
}

func TestSnapshotScheduleConcurrentEnableCreatesOnce(t *testing.T) {
	m, u, f := newSnapshotManager(t, 2)
	other := New(m.cfg, m.db)
	entered, release := make(chan struct{}), make(chan struct{})
	f.entered, f.release = entered, release
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- m.SetSnapshotSchedule(u.Name, true, 1) }()
	<-entered
	go func() { second <- other.SetSnapshotSchedule(u.Name, true, 1) }()
	select {
	case err := <-second:
		close(release)
		t.Fatalf("duplicate enable crossed the first creation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 1 || len(f.snaps) != 1 {
		t.Fatalf("concurrent enable created duplicate checkpoints: %v", f.calls)
	}
}

func TestSnapshotScheduleSinglePosition(t *testing.T) {
	m, u, f := newSnapshotManager(t, 1)
	if err := m.SetSnapshotSchedule(u.Name, true, 1); err != nil {
		t.Fatal(err)
	}
	if err := m.SnapshotCreate(u.Name); err == nil {
		t.Fatal("manual create consumed the reserved position")
	}
	if err := runSnapshotDue(t, m, u); err != nil {
		t.Fatal(err)
	}
	if len(f.snaps) != 1 {
		t.Fatal("missing automatic checkpoint")
	}
	if err := m.SnapshotPromote(u.Name, f.snaps[0].Name); err == nil {
		t.Fatal("promotion consumed the reserved position")
	}
	if err := m.SetSnapshotSchedule(u.Name, false, 0); err != nil {
		t.Fatal(err)
	}
	if err := m.SetSnapshotSchedule(u.Name, true, 1); err == nil {
		t.Fatal("re-enabled without freeing the only position")
	}
}

func TestAutomaticSnapshotRotationAndNoCatchUp(t *testing.T) {
	m, u, f := newSnapshotManager(t, 5)
	seedCheckpoints(t, m, u, f, 1, 4)
	if err := runSnapshotDue(t, m, u); err != nil {
		t.Fatal(err)
	}
	if len(f.snaps) != 5 || len(f.calls) != 2 || !strings.HasPrefix(f.calls[0], "create:auto-") || f.calls[1] != "delete:auto-0" {
		t.Fatalf("rotation must create before deleting oldest auto: %v, snaps=%v", f.calls, f.snaps)
	}
	if err := m.RunScheduledSnapshots(time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 {
		t.Fatal("same deadline ran twice")
	}
	// A new Manager sees durable metadata/deadlines rather than running again.
	restarted := New(m.cfg, m.db)
	if err := restarted.RunScheduledSnapshots(time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 2 {
		t.Fatal("restart repeated a completed task")
	}
	snaps, _ := restarted.SnapshotList(u.Name)
	if fixedSnapshotCount(snaps) != 1 {
		t.Fatal("restart lost automatic classification")
	}
	if err := m.db.SaveSnapshotSchedule(u.ID, true, 1, time.Now().Add(-24*time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	if err := m.RunScheduledSnapshots(time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 4 {
		t.Fatal("missed intervals were replayed instead of one attempt")
	}
}

func TestAutomaticSnapshotFailureRecovery(t *testing.T) {
	m, u, f := newSnapshotManager(t, 2)
	seedCheckpoints(t, m, u, f, 1, 1)
	f.createFail = true
	if err := runSnapshotDue(t, m, u); err == nil {
		t.Fatal("creation failure was hidden")
	}
	if len(f.snaps) != 2 || len(f.calls) != 1 {
		t.Fatal("failed creation deleted an existing recovery point")
	}
	f.createFail = false
	f.deleteFail = true
	if err := runSnapshotDue(t, m, u); err == nil {
		t.Fatal("rotation failure was hidden")
	}
	if len(f.snaps) != 3 {
		t.Fatal("expected recoverable transient excess")
	}
	schedule, _ := m.SnapshotSchedule(u.Name)
	if !strings.Contains(schedule.LastError, "created; rotation failed") || schedule.LastSuccessAt == 0 {
		t.Fatalf("creation/cleanup result is ambiguous: %+v", schedule)
	}
	calls := len(f.calls)
	if err := runSnapshotDue(t, m, u); err == nil {
		t.Fatal("failed excess repair was hidden")
	}
	if len(f.snaps) != 3 || len(f.calls) != calls+1 || !strings.HasPrefix(f.calls[len(f.calls)-1], "delete:") {
		t.Fatal("failed cleanup allowed unbounded snapshot growth")
	}
	f.deleteFail = false
	if err := runSnapshotDue(t, m, u); err != nil {
		t.Fatal(err)
	}
	if len(f.snaps) != 2 || f.snaps[0].Name != "fixed-0" {
		t.Fatal("recovery failed or removed a fixed checkpoint")
	}
}

func TestAutomaticSnapshotSkipsUnsafeStates(t *testing.T) {
	for _, state := range []string{"expired", db.StatusCreating, db.StatusFailed, db.StatusReinstalling, "pool-full", "list-failed", "quota-reduced", "disabled"} {
		t.Run(state, func(t *testing.T) {
			m, u, f := newSnapshotManager(t, 3)
			seedCheckpoints(t, m, u, f, 2, 0)
			switch state {
			case "expired":
				if err := m.db.UpdateUserExpiry(u.ID, time.Now().Add(-time.Hour).Format(time.RFC3339)); err != nil {
					t.Fatal(err)
				}
			case db.StatusCreating, db.StatusFailed, db.StatusReinstalling:
				if err := m.db.UpdateUserStatus(u.ID, state); err != nil {
					t.Fatal(err)
				}
			case "pool-full":
				f.usage = 90
			case "list-failed":
				f.listFail = true
			case "quota-reduced":
				if err := m.db.UpdateSnapshotLimit(u.ID, 2); err != nil {
					t.Fatal(err)
				}
			case "disabled":
				if err := m.db.UpdateSnapshotLimit(u.ID, 0); err != nil {
					t.Fatal(err)
				}
			}
			if err := runSnapshotDue(t, m, u); err == nil {
				t.Fatal("unsafe execution was accepted")
			}
			if len(f.calls) != 0 {
				t.Fatalf("unsafe execution changed snapshots: %v", f.calls)
			}
			schedule, _ := m.SnapshotSchedule(u.Name)
			if schedule.LastError == "" || schedule.NextRunAt <= time.Now().Unix() {
				t.Fatal("skip was not persisted or would retry in a tight loop")
			}
		})
	}
}

func TestAutomaticSnapshotSharingAndRestore(t *testing.T) {
	m, u, f := newSnapshotManager(t, 5)
	seedCheckpoints(t, m, u, f, 1, 3)
	if _, _, err := m.ShareSnapshot(u.Name, "auto-1"); err == nil {
		t.Fatal("automatic checkpoint was directly shared")
	}
	if err := m.SnapshotPromote(u.Name, "auto-1"); err != nil {
		t.Fatal(err)
	}
	_, removed, err := m.ShareSnapshot(u.Name, "auto-1")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(removed, []string{"fixed-0", "auto-0"}) {
		t.Fatalf("sharing did not remove all older types: %v", removed)
	}
	automatic, _ := m.db.AutomaticSnapshots(u.ID)
	if automatic["auto-0"] || automatic["auto-1"] || !automatic["auto-2"] {
		t.Fatalf("stale classification after share/promotion: %v", automatic)
	}
	// Point the share at a newer fixed checkpoint, then restore an automatic
	// one before it. This must invalidate the removed newer share mapping too.
	if err := m.SnapshotPromote(u.Name, "auto-2"); err != nil {
		t.Fatal(err)
	}
	if err := m.db.SetSnapshotShare(u.ID, "newer-share", "auto-2"); err != nil {
		t.Fatal(err)
	}
	// auto-1 is fixed from the earlier promotion; add a new automatic target
	// before auto-2 to exercise restore permission independently of sharing.
	if err := m.db.BeginAutomaticSnapshot(u.ID, "restore-auto", 0, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	f.snaps = append(f.snaps, lx.SnapshotInfo{Name: "restore-auto", CreatedAt: snapshotCreateTime(f.snaps[0]).Add(time.Millisecond).Format(time.RFC3339Nano)})
	f.running = true
	if err := m.SnapshotRestore(u.Name, "restore-auto"); err != nil {
		t.Fatal(err)
	}
	if !f.running || len(f.snaps) != 2 {
		t.Fatalf("restore lost original power state or kept later points: %v", f.snaps)
	}
	if share, _ := m.db.GetSnapshotShare(u.ID); share != nil {
		t.Fatal("restore left a share pointing at a removed later checkpoint")
	}
	automatic, _ = m.db.AutomaticSnapshots(u.ID)
	if !automatic["restore-auto"] {
		t.Fatal("restore target lost its automatic classification")
	}
}

func TestSnapshotDisableWaitsForInFlightCreation(t *testing.T) {
	m, u, f := newSnapshotManager(t, 2)
	seedCheckpoints(t, m, u, f, 0, 1)
	entered := make(chan struct{})
	f.entered, f.release = entered, make(chan struct{})
	runDone := make(chan error, 1)
	go func() { runDone <- runSnapshotDue(t, m, u) }()
	<-entered
	disableDone := make(chan error, 1)
	go func() { disableDone <- m.SetSnapshotSchedule(u.Name, false, 0) }()
	select {
	case err := <-disableDone:
		close(f.release)
		t.Fatalf("disable crossed in-flight creation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(f.release)
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if err := <-disableDone; err != nil {
		t.Fatal(err)
	}
	snaps, err := m.SnapshotList(u.Name)
	if err != nil || fixedSnapshotCount(snaps) != len(snaps) {
		t.Fatalf("automatic checkpoint appeared after disabling: %v %v", snaps, err)
	}
}
