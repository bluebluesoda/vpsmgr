package mgr

import (
	"path/filepath"
	"strings"
	"testing"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
	"vpsmgr/internal/lx"
)

func TestNewShareCode(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		code, err := newShareCode()
		if err != nil {
			t.Fatal(err)
		}
		if len(code) != 36 {
			t.Fatalf("code %q has length %d, want 36 (UUID)", code, len(code))
		}
		if code[14] != '4' {
			t.Errorf("code %q is not a v4 UUID (version nibble %q)", code, code[14])
		}
		if v := code[19]; v != '8' && v != '9' && v != 'a' && v != 'b' {
			t.Errorf("code %q has a bad variant nibble %q", code, v)
		}
		if seen[code] {
			t.Fatalf("duplicate share code %q", code)
		}
		seen[code] = true
	}
}

func TestOlderSnapshotNames(t *testing.T) {
	snaps := []lx.SnapshotInfo{
		{Name: "old", CreatedAt: "2026-08-01T10:00:00Z"},
		{Name: "mid", CreatedAt: "2026-08-02T10:00:00Z"},
		{Name: "new", CreatedAt: "2026-08-03T10:00:00Z"},
		{Name: "bad", CreatedAt: "not-a-time"},
	}
	got := olderSnapshotNames(snaps, "mid")
	if len(got) != 1 || got[0] != "old" {
		t.Fatalf("olderSnapshotNames(mid) = %v, want [old]", got)
	}
	if got := olderSnapshotNames(snaps, "old"); len(got) != 0 {
		t.Fatalf("olderSnapshotNames(old) = %v, want none", got)
	}
	// An unknown target deletes nothing (safe).
	if got := olderSnapshotNames(snaps, "ghost"); got != nil {
		t.Fatalf("olderSnapshotNames(ghost) = %v, want nil", got)
	}
}

func TestIsSubsequentSnapshotErr(t *testing.T) {
	yes := []string{
		`Snapshot "s1" cannot be restored due to subsequent snapshot(s). Set zfs.remove_snapshots to override`,
		`Snapshot "s1" cannot be restored due to subsequent internal snapshot(s) (from a copy)`,
	}
	for _, m := range yes {
		if !isSubsequentSnapshotErr(errString(m)) {
			t.Errorf("not matched as subsequent-snapshot error: %q", m)
		}
	}
	if isSubsequentSnapshotErr(errString("some other failure")) {
		t.Error("unrelated error matched as subsequent-snapshot")
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestShareSnapshotRejectsBadInput(t *testing.T) {
	c := cfg.Default()
	c.Incus.Socket = "/nonexistent/vpsmgr-test.sock"
	d, err := db.Open(filepath.Join(t.TempDir(), "share.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 10, 1024, 10); err != nil {
		t.Fatal(err)
	}
	m := New(c, d)

	// Invalid snapshot name is rejected before any Incus call.
	if _, _, err := m.ShareSnapshot("alice", "../evil"); err == nil {
		t.Fatal("invalid snapshot name accepted")
	}
	// A valid name reaches Incus, which is unreachable here → error.
	if _, _, err := m.ShareSnapshot("alice", "snap-x"); err == nil {
		t.Fatal("expected an error: Incus is unreachable")
	}
}

func TestSnapshotShareInfoAndRevoke(t *testing.T) {
	c := cfg.Default()
	c.Incus.Socket = "/nonexistent/vpsmgr-test.sock"
	d, err := db.Open(filepath.Join(t.TempDir(), "share2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	u, err := d.CreateUser("bob", "h", "10.115.0.3", 2, 30002, 10100, 10, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	m := New(c, d)

	if _, _, ok := m.SnapshotShareInfo("bob"); ok {
		t.Fatal("reported a share before one exists")
	}
	if err := d.SetSnapshotShare(u.ID, "code-x", "snap-1"); err != nil {
		t.Fatal(err)
	}
	code, snap, ok := m.SnapshotShareInfo("bob")
	if !ok || code != "code-x" || snap != "snap-1" {
		t.Fatalf("SnapshotShareInfo = %q,%q,%v", code, snap, ok)
	}
	if err := m.RevokeSnapshotShare("bob"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := m.SnapshotShareInfo("bob"); ok {
		t.Fatal("share still reported after revoke")
	}
}

func TestSnapshotSharingToggleEnforced(t *testing.T) {
	c := cfg.Default()
	c.Incus.Socket = "/nonexistent/vpsmgr-test.sock"
	d, err := db.Open(filepath.Join(t.TempDir(), "share-toggle.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.CreateUser("dave", "h", "10.115.0.5", 4, 30004, 10300, 10, 1024, 10); err != nil {
		t.Fatal(err)
	}
	m := New(c, d)

	// Default is enabled (the feature is on after an upgrade).
	if !m.SnapshotShareEnabled() {
		t.Fatal("sharing should default to enabled")
	}
	if err := m.SetSnapshotShareEnabled(false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.ShareSnapshot("dave", "snap-x"); err == nil ||
		!strings.Contains(err.Error(), "disabled") {
		t.Fatalf("share while disabled = %v, want a disabled error", err)
	}
	if _, err := m.ReinstallFromShare("dave", "any-code"); err == nil ||
		!strings.Contains(err.Error(), "disabled") {
		t.Fatalf("install while disabled = %v, want a disabled error", err)
	}
	if err := m.SetSnapshotShareEnabled(true); err != nil {
		t.Fatal(err)
	}
	if !m.SnapshotShareEnabled() {
		t.Fatal("re-enable did not stick")
	}
}

func TestReinstallFromShareUnknownCode(t *testing.T) {
	c := cfg.Default()
	c.Incus.Socket = "/nonexistent/vpsmgr-test.sock"
	d, err := db.Open(filepath.Join(t.TempDir(), "share3.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.CreateUser("carol", "h", "10.115.0.4", 3, 30003, 10200, 10, 1024, 10); err != nil {
		t.Fatal(err)
	}
	m := New(c, d)

	if _, err := m.ReinstallFromShare("carol", "not-a-real-code"); err == nil ||
		!strings.Contains(err.Error(), "invalid or expired") {
		t.Fatalf("unknown code error = %v, want invalid/expired", err)
	}
}
