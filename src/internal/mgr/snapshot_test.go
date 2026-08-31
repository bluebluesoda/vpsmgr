package mgr

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
	"vpsmgr/internal/lx"
)

func TestValidSnapName(t *testing.T) {
	valid := []string{
		"snap-20260820-153000",
		"a",
		"a.b_c-d",
		"snap1",
	}
	invalid := []string{
		"", "-a", ".a", "../evil", "a/b", "a b", "a+b", "a\\b",
		strings.Repeat("x", 65), // Incus name length limit
	}
	for _, n := range valid {
		if !ValidSnapName(n) {
			t.Errorf("ValidSnapName(%q) = false, want true", n)
		}
	}
	for _, n := range invalid {
		if ValidSnapName(n) {
			t.Errorf("ValidSnapName(%q) = true, want false", n)
		}
	}
}

func TestSnapNameFormat(t *testing.T) {
	n := snapName()
	if !strings.HasPrefix(n, "snap-") {
		t.Errorf("snapName() = %q, want snap- prefix", n)
	}
	if !ValidSnapName(n) {
		t.Errorf("snapName() = %q does not pass ValidSnapName", n)
	}
	// snap-YYYYMMDD-HHMMSS-XXXX = 8+1+6+1+4 = 20 chars after the prefix.
	if len(n) != len("snap-")+20 {
		t.Errorf("snapName() = %q, want snap-<20 chars>", n)
	}
	// Two calls in the same second must differ (random suffix).
	if snapName() == snapName() {
		t.Error("snapName() returned the same value twice; concurrent creates would collide")
	}
}

// TestSnapshotCreateDisabled verifies that a snapshot cap of 0 refuses creation
// outright (before any Incus call), while leaving existing snapshots untouched.
func TestSnapshotCreateDisabled(t *testing.T) {
	c := cfg.Default()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.CreateUser("alice", "x", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	m := New(c, d)
	// A dead socket means any real Incus call would fail; with the cap disabled
	// we must refuse before reaching Incus at all.
	c.Incus.Socket = "/nonexistent/vpsmgr-test.sock"
	c.Snapshots.Limit = 0
	if err := m.SnapshotCreate("alice"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("SnapshotCreate with limit 0 = %v, want 'snapshots are disabled'", err)
	}
}

// snapInfo is a test helper building a SnapshotInfo with an RFC3339Nano time.
func snapInfo(i int, name string, nano int64) lx.SnapshotInfo {
	t := time.Unix(int64(1700000000+i), nano).UTC()
	return lx.SnapshotInfo{Name: name, CreatedAt: t.Format(time.RFC3339Nano)}
}

// TestNewerSnapshotNames verifies restore pre-cleanup: restoring to an older
// checkpoint must discard every snapshot created strictly after it, keep the
// target and anything older, and tolerate unparseable timestamps.
func TestNewerSnapshotNames(t *testing.T) {
	// SnapshotList returns newest-first, matching the list.newerSnapshotNames
	// keeps that order, so deleting "newest first" is the natural degradation.
	snaps := []lx.SnapshotInfo{
		snapInfo(3, "snap-C", 0), // newest
		snapInfo(2, "snap-B", 0),
		snapInfo(1, "snap-A", 0), // target
	}
	// Restore to A: B and C are newer and must be removed (newest-first).
	if got, want := newerSnapshotNames(snaps, "snap-A"), []string{"snap-C", "snap-B"}; !reflect.DeepEqual(got, want) {
		t.Errorf("newerSnapshotNames(A) = %v, want %v", got, want)
	}
	// Restore to B: only C is newer.
	if got, want := newerSnapshotNames(snaps, "snap-B"), []string{"snap-C"}; !reflect.DeepEqual(got, want) {
		t.Errorf("newerSnapshotNames(B) = %v, want %v", got, want)
	}
	// Restore to the newest: nothing to delete.
	if got := newerSnapshotNames(snaps, "snap-C"); got != nil {
		t.Errorf("newerSnapshotNames(C) = %v, want nil", got)
	}
	// Missing target: delete nothing, let the restore report the error.
	if got := newerSnapshotNames(snaps, "nope"); got != nil {
		t.Errorf("newerSnapshotNames(missing) = %v, want nil", got)
	}
	// A snapshot whose time cannot be parsed is ignored, not deleted.
	broken := []lx.SnapshotInfo{
		{Name: "snap-C", CreatedAt: "not-a-time"},
		snapInfo(1, "snap-A", 0),
	}
	if got := newerSnapshotNames(broken, "snap-A"); got != nil {
		t.Errorf("newerSnapshotNames(broken) = %v, want nil", got)
	}
	// Subsecond discrimination: a same-second snapshot with a larger fractional
	// time is still newer.
	sub := []lx.SnapshotInfo{
		snapInfo(1, "snap-A", 123),
		snapInfo(1, "snap-B", 999),
	}
	if got, want := newerSnapshotNames(sub, "snap-A"), []string{"snap-B"}; !reflect.DeepEqual(got, want) {
		t.Errorf("newerSnapshotNames(subsec) = %v, want %v", got, want)
	}
}
