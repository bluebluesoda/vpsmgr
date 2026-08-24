package mgr

import (
	"path/filepath"
	"strings"
	"testing"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
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
