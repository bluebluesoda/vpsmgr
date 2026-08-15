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
	c.Net.ExtIF = "eth0" // WireIPv6Pool / RewireAllIPv6Pool needs it
	d, err := db.Open(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	// Point config Save/Load at a temp file so AddPoolIPv6s etc. don't touch
	// /etc/vpsmgr.
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	t.Setenv("VPSMGR_CONFIG", cfgPath)
	if err := cfg.Save(c); err != nil {
		t.Fatal(err)
	}
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

func TestAddPoolIPv6s(t *testing.T) {
	m, _ := poolTestManager(t, nil)
	added, err := m.AddPoolIPv6s([]string{"2001:db8::1", "2001:db8::2/128", "2001:db8::2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(added) != 2 {
		t.Fatalf("added = %v, want 2 unique addresses", added)
	}
	// The /128 form is normalized to the bare address.
	pool, err := m.cfg.IPv6PoolValidated()
	if err != nil {
		t.Fatal(err)
	}
	if len(pool) != 2 || pool[0] != "2001:db8::1" || pool[1] != "2001:db8::2" {
		t.Errorf("pool after add = %v, want [2001:db8::1 2001:db8::2]", pool)
	}

	// Invalid entry (bad prefix) rejects the whole batch.
	if _, err := m.AddPoolIPv6s([]string{"2001:db8::9/64"}); err == nil {
		t.Error("expected rejection of /64 entry")
	}
	// Private/ULA rejected.
	if _, err := m.AddPoolIPv6s([]string{"fc00::1"}); err == nil {
		t.Error("expected rejection of ULA")
	}
}

func TestRemovePoolIPv6(t *testing.T) {
	m, d := poolTestManager(t, []string{"2001:db8::1", "2001:db8::2"})
	// Assigned address cannot be removed.
	if _, err := d.CreateUserFull("alice", "h", "10.42.0.2", 1, 30001, 10000, 1, 1024, 10, 0, db.StatusReady, "2001:db8::1"); err != nil {
		t.Fatal(err)
	}
	if err := m.RemovePoolIPv6("2001:db8::1"); err == nil {
		t.Error("expected error removing an assigned address")
	}
	// Unknown address errors.
	if err := m.RemovePoolIPv6("2001:db8::99"); err == nil {
		t.Error("expected error removing an unknown address")
	}
	// Free address removed fine.
	if err := m.RemovePoolIPv6("2001:db8::2"); err != nil {
		t.Fatal(err)
	}
	pool, _ := m.cfg.IPv6PoolValidated()
	if len(pool) != 1 || pool[0] != "2001:db8::1" {
		t.Errorf("pool after remove = %v, want [2001:db8::1]", pool)
	}
}

func TestPoolList(t *testing.T) {
	m, d := poolTestManager(t, []string{"2001:db8::1", "2001:db8::2"})
	if _, err := d.CreateUserFull("alice", "h", "10.42.0.2", 1, 30001, 10000, 1, 1024, 10, 0, db.StatusReady, "2001:db8::1"); err != nil {
		t.Fatal(err)
	}
	entries, err := m.PoolList()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	if !entries[0].Used || entries[0].User != "alice" {
		t.Errorf("entries[0] = %+v, want used by alice", entries[0])
	}
	if entries[1].Used {
		t.Errorf("entries[1] should be free: %+v", entries[1])
	}
}
