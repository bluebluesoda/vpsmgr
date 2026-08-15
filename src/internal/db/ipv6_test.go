package db

import (
	"path/filepath"
	"testing"
)

// TestIPv6AddressLifecycle: a pool address is stored with the user, released
// on delete, and the UNIQUE index refuses a double assignment.
func TestIPv6AddressLifecycle(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "v6.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	addr := "2001:db8::9c4"
	u, err := d.CreateUserFull("alice", "h", "10.42.0.2", 1, 30001, 10000, 1, 1024, 10, 0, StatusReady, addr)
	if err != nil {
		t.Fatal(err)
	}
	if u.IPv6Address != addr {
		t.Fatalf("stored address = %q, want %q", u.IPv6Address, addr)
	}

	// Same address for a second user must be refused (UNIQUE index).
	if _, err := d.CreateUserFull("bob", "h", "10.42.0.3", 2, 30002, 10100, 1, 1024, 10, 0, StatusReady, addr); err == nil {
		t.Fatal("expected UNIQUE violation for duplicate address, got nil")
	}

	// UsedIPv6Addresses sees it.
	used, err := d.UsedIPv6Addresses()
	if err != nil {
		t.Fatal(err)
	}
	if !used[addr] {
		t.Errorf("used set missing %q", addr)
	}

	// Delete releases the address.
	if err := d.DeleteUser(u.ID); err != nil {
		t.Fatal(err)
	}
	used, _ = d.UsedIPv6Addresses()
	if used[addr] {
		t.Error("address still used after user delete")
	}

	// The address is now assignable again.
	if _, err := d.CreateUserFull("carol", "h", "10.42.0.4", 3, 30003, 10200, 1, 1024, 10, 0, StatusReady, addr); err != nil {
		t.Fatalf("reassign after delete: %v", err)
	}
}

// TestIPv6AddressNullShared: many users may have no address (NULL), the
// UNIQUE index must not treat them as equal.
func TestIPv6AddressNullShared(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "v6null.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for i, name := range []string{"a", "b", "c"} {
		if _, err := d.CreateUserFull(name, "h", "10.42.0."+string(rune('2'+i)), i+1, 30001+i, 10000+i*100, 1, 1024, 10, 0, StatusReady, ""); err != nil {
			t.Fatalf("user %s without address: %v", name, err)
		}
	}
}
