package db

import (
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLockOperationsAcrossDBInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "operations.db")
	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	unlock, err := first.LockOperations()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	// A separately opened FD must see the same cross-process sidecar flock.
	probe, err := os.Open(path + ".operations.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("database inode is not flock-locked: %v", err)
	}
	// SQLite fcntl writes still work while the operation flock is held.
	if err := second.SetSetting("operation-lock-test", "ok"); err != nil {
		t.Fatalf("operation flock interfered with SQLite: %v", err)
	}

	type result struct {
		unlock func()
		err    error
	}
	started := make(chan struct{})
	acquired := make(chan result, 1)
	go func() {
		close(started)
		u, err := second.LockOperations()
		acquired <- result{u, err}
	}()
	<-started
	select {
	case r := <-acquired:
		if r.unlock != nil {
			r.unlock()
		}
		t.Fatalf("second DB did not block: %v", r.err)
	case <-time.After(40 * time.Millisecond):
	}
	unlock()
	unlock() // idempotent, including when another caller acquires next
	select {
	case r := <-acquired:
		if r.err != nil {
			t.Fatal(r.err)
		}
		defer r.unlock()
		unlock() // a stale unlock must not close/release a newer FD
		if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
			t.Fatalf("second lock was not retained: %v", err)
		}
		r.unlock()
		r.unlock()
	case <-time.After(2 * time.Second):
		t.Fatal("second lock did not acquire after release")
	}
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("unlock did not release kernel lock: %v", err)
	}
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "operations.db" && entry.Name() != "operations.db-wal" && entry.Name() != "operations.db-shm" && entry.Name() != "operations.db.operations.lock" {
			t.Errorf("unexpected sidecar file: %s", entry.Name())
		}
	}
}

func TestLockOperationsOpenError(t *testing.T) {
	d := snapshotTestDB(t)
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(d.path); err != nil {
		t.Fatal(err)
	}
	unlock, err := d.LockOperations()
	if err == nil || unlock != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file lock = %v, unlock nil=%v", err, unlock == nil)
	}
}

func TestOperationUnlockPreservesSQLiteLocks(t *testing.T) {
	if path := os.Getenv("VPSMGR_LOCK_TEST_CHILD"); path != "" {
		raw, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(0)")
		if err != nil {
			t.Fatal(err)
		}
		defer raw.Close()
		_, err = raw.Exec(`INSERT INTO settings(key,value) VALUES('child-write','unsafe')`)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "locked") {
			t.Fatalf("SQLite writer lock was released by operation unlock: %v", err)
		}
		return
	}
	d := snapshotTestDB(t)
	if _, err := d.sql.Exec(`PRAGMA journal_mode=DELETE`); err != nil {
		t.Fatal(err)
	}
	tx, err := d.sql.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO settings(key,value) VALUES('parent-write','held')`); err != nil {
		t.Fatal(err)
	}
	unlock, err := d.LockOperations()
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	cmd := exec.Command(os.Args[0], "-test.run=^TestOperationUnlockPreservesSQLiteLocks$", "-test.timeout=10s")
	cmd.Env = append(os.Environ(), "VPSMGR_LOCK_TEST_CHILD="+d.path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("SQLite lock regression: %v\n%s", err, out)
	}
}
