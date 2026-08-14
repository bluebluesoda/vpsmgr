package mgr

import (
	"path/filepath"
	"testing"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
)

// TestUpdateQuotasAndBandwidthRollsBackBandwidthOnResourceFailure verifies the
// combined admin quota submit cannot half-apply: the bandwidth quota is written
// first, and when the Incus-side resource update fails it is restored. Incus is
// pointed at a nonexistent socket so the resource update always fails.
func TestUpdateQuotasAndBandwidthRollsBackBandwidthOnResourceFailure(t *testing.T) {
	c := cfg.Default()
	c.Incus.Socket = "/nonexistent/vpsmgr-test.sock"
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 10, 1024, 10); err != nil {
		t.Fatal(err)
	}
	m := New(c, d)

	if _, err := m.UpdateQuotasAndBandwidth("alice", 20, 2048, 20, 100); err == nil {
		t.Fatal("expected an error: Incus is unreachable")
	}
	u, err := d.GetUserByName("alice")
	if err != nil {
		t.Fatal(err)
	}
	if u.BandwidthQuotaGB != 0 {
		t.Errorf("bandwidth quota not rolled back: got %d, want 0 (unchanged)", u.BandwidthQuotaGB)
	}
	if u.CPU != 10 || u.MemMB != 1024 || u.DiskGB != 10 {
		t.Errorf("resource quotas changed despite failure: cpu=%d mem=%d disk=%d", u.CPU, u.MemMB, u.DiskGB)
	}
}

// TestUpdateQuotasValidatesBeforeApplying ensures an invalid value is rejected
// before anything touches Incus, so a bad submit never leaves a partial change.
func TestUpdateQuotasValidatesBeforeApplying(t *testing.T) {
	c := cfg.Default()
	c.Incus.Socket = "/nonexistent/vpsmgr-test.sock"
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.CreateUser("bob", "h", "10.115.0.3", 2, 30002, 10100, 10, 1024, 10); err != nil {
		t.Fatal(err)
	}
	m := New(c, d)

	// Disk shrink is rejected up front (no Incus call is made).
	if _, err := m.UpdateQuotas("bob", 20, 2048, 5); err == nil {
		t.Fatal("expected disk-shrink error")
	}
	// Invalid CPU rejected up front.
	if _, err := m.UpdateQuotas("bob", 11, 2048, 20); err == nil {
		t.Fatal("expected invalid-CPU error")
	}
	u, err := d.GetUserByName("bob")
	if err != nil {
		t.Fatal(err)
	}
	if u.CPU != 10 || u.MemMB != 1024 || u.DiskGB != 10 {
		t.Errorf("quotas changed despite validation failure: cpu=%d mem=%d disk=%d", u.CPU, u.MemMB, u.DiskGB)
	}
}
