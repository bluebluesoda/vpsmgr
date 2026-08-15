package mgr

import (
	"path/filepath"
	"testing"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
)

func poolTestManager(t *testing.T, addrs []string) (*Manager, *db.DB) {
	t.Helper()
	c := cfg.Default()
	c.Net.IPv6Mode = cfg.IPv6ModePool
	c.Net.IPv6Pool = addrs
	d, err := db.Open(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return &Manager{cfg: c, db: d}, d
}

func TestPickPoolIPv6Auto(t *testing.T) {
	m, _ := poolTestManager(t, []string{"2001:db8::1", "2001:db8::2"})
	got, err := m.pickPoolIPv6("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "2001:db8::1" {
		t.Errorf("auto pick = %q, want first free 2001:db8::1", got)
	}
}

func TestPickPoolIPv6ExplicitAndUsed(t *testing.T) {
	m, d := poolTestManager(t, []string{"2001:db8::1", "2001:db8::2"})
	// Reserve ::2 by creating a user with it.
	if _, err := d.CreateUserFull("alice", "h", "10.42.0.2", 1, 30001, 10000, 1, 1024, 10, 0, db.StatusReady, "2001:db8::2"); err != nil {
		t.Fatal(err)
	}
	// Explicitly picking a used address fails.
	if _, err := m.pickPoolIPv6("2001:db8::2"); err == nil {
		t.Error("expected error for used address")
	}
	// Picking an address outside the pool fails.
	if _, err := m.pickPoolIPv6("2001:db8::99"); err == nil {
		t.Error("expected error for address outside pool")
	}
	// Auto now returns the still-free one.
	got, err := m.pickPoolIPv6("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "2001:db8::1" {
		t.Errorf("auto after reservation = %q, want 2001:db8::1", got)
	}
}

func TestPickPoolIPv6Exhausted(t *testing.T) {
	m, d := poolTestManager(t, []string{"2001:db8::1"})
	if _, err := d.CreateUserFull("alice", "h", "10.42.0.2", 1, 30001, 10000, 1, 1024, 10, 0, db.StatusReady, "2001:db8::1"); err != nil {
		t.Fatal(err)
	}
	got, err := m.pickPoolIPv6("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("exhausted pool auto = %q, want \"\" (V4-only)", got)
	}
}

func TestFreePoolIPv6List(t *testing.T) {
	m, d := poolTestManager(t, []string{"2001:db8::1", "2001:db8::2", "2001:db8::3"})
	if _, err := d.CreateUserFull("alice", "h", "10.42.0.2", 1, 30001, 10000, 1, 1024, 10, 0, db.StatusReady, "2001:db8::2"); err != nil {
		t.Fatal(err)
	}
	free := m.FreePoolIPv6List()
	if len(free) != 2 {
		t.Fatalf("free list len = %d, want 2 (%v)", len(free), free)
	}
	if free[0] != "2001:db8::1" || free[1] != "2001:db8::3" {
		t.Errorf("free list = %v, want [2001:db8::1 2001:db8::3]", free)
	}
}

func TestIPv6PoolUsage(t *testing.T) {
	m, d := poolTestManager(t, []string{"2001:db8::1", "2001:db8::2"})
	total, used, err := m.IPv6PoolUsage()
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || used != 0 {
		t.Errorf("empty pool usage = %d/%d, want 2/0", used, total)
	}
	if _, err := d.CreateUserFull("alice", "h", "10.42.0.2", 1, 30001, 10000, 1, 1024, 10, 0, db.StatusReady, "2001:db8::1"); err != nil {
		t.Fatal(err)
	}
	total, used, _ = m.IPv6PoolUsage()
	if total != 2 || used != 1 {
		t.Errorf("after reservation usage = %d/%d, want 2/1", used, total)
	}
}
