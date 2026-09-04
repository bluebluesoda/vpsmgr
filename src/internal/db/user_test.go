package db

import (
	"fmt"
	"testing"

	"vpsmgr/internal/cfg"
)

// TestNextFreeIdxRandom verifies NextFreeIdx returns an unused index within
// [1, cfg.MaxUsers] (never a used one), and that it differs from a guaranteed
// free index when other slots are taken.
func TestNextFreeIdxRandom(t *testing.T) {
	d, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateUser("bob", "h", "10.115.0.3", 2, 30002, 10100, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}

	// With idx 1 and 2 used, a random pick must never return them and must
	// stay inside the valid range. idx 200 is guaranteed free, so over many
	// picks at least some should be a non-trivial spread across the range.
	seen := map[int]bool{}
	for i := 0; i < 50; i++ {
		n, err := d.NextFreeIdx(1, cfg.MaxUsers)
		if err != nil {
			t.Fatal(err)
		}
		if n < 1 || n > cfg.MaxUsers {
			t.Fatalf("NextFreeIdx = %d, out of range 1..%d", n, cfg.MaxUsers)
		}
		if n == 1 || n == 2 {
			t.Fatalf("NextFreeIdx = %d, already used", n)
		}
		seen[n] = true
	}
	if len(seen) < 10 {
		t.Fatalf("NextFreeIdx not random: only %d distinct values in 50 picks", len(seen))
	}
}

// TestUserColorRoundTrip exercises the per-user accent color: it defaults to
// empty, persists across a set, and an empty value resets it. Also covers the
// ListUsers scan path which reads the same column.
func TestUserColorRoundTrip(t *testing.T) {
	d, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	u, err := d.CreateUser("dave", "h", "10.115.0.5", 4, 30004, 10300, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	if u.Color != "" {
		t.Errorf("new user color = %q, want empty default", u.Color)
	}
	if err := d.UpdateUserColor(u.ID, "#e11d48"); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetUserByID(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Color != "#e11d48" {
		t.Errorf("color after set = %q, want #e11d48", got.Color)
	}
	byName, err := d.GetUserByName("dave")
	if err != nil {
		t.Fatal(err)
	}
	if byName.Color != "#e11d48" {
		t.Errorf("color via name = %q, want #e11d48", byName.Color)
	}
	if err := d.UpdateUserColor(u.ID, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = d.GetUserByID(u.ID)
	if got.Color != "" {
		t.Errorf("color after reset = %q, want empty", got.Color)
	}
	list, err := d.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Color != "" {
		t.Errorf("ListUsers color = %+v, want one user with empty color", list)
	}
}
func TestNextFreeIdxExhausted(t *testing.T) {
	d, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	for i := 1; i <= cfg.MaxUsers; i++ {
		if _, err := d.CreateUser(fmt.Sprintf("u%d", i), "h", "10.115.0.2", i, 30000+i, 10000+(i-1)*100, 1, 1024, 10); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.NextFreeIdx(1, cfg.MaxUsers); err == nil {
		t.Fatal("NextFreeIdx on a full pool: expected error, got nil")
	}
}

// TestUsedStartPorts verifies UsedStartPorts returns exactly the start_port
// values already assigned — the source of truth for the port-block allocator
// (which is now independent of idx).
func TestUsedStartPorts(t *testing.T) {
	d, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateUser("bob", "h", "10.115.0.3", 2, 30002, 15000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	used, err := d.UsedStartPorts()
	if err != nil {
		t.Fatal(err)
	}
	if len(used) != 2 || !used[10000] || !used[15000] {
		t.Errorf("UsedStartPorts = %v, want {10000,15000}", used)
	}
}

// TestStartPortUnique verifies the v13 UNIQUE index on start_port refuses a
// duplicate block even when idx differs (the port-block allocator's backstop).
func TestStartPortUnique(t *testing.T) {
	d, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateUser("bob", "h", "10.115.0.3", 2, 30002, 10000, 1, 1024, 10); err == nil {
		t.Fatal("duplicate start_port with a different idx: expected UNIQUE violation, got nil")
	}
}
