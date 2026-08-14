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
		n, err := d.NextFreeIdx()
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

// TestNextFreeIdxExhausted verifies the error path when every index is taken.
func TestNextFreeIdxExhausted(t *testing.T) {
	d, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	for i := 1; i <= cfg.MaxUsers; i++ {
		if _, err := d.CreateUser(fmt.Sprintf("u%d", i), "h", "10.115.0.2", i, 30000+i, 10000, 1, 1024, 10); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.NextFreeIdx(); err == nil {
		t.Fatal("NextFreeIdx on a full pool: expected error, got nil")
	}
}
