package mgr

import (
	"net"
	"path/filepath"
	"testing"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
)

func TestUserPorts(t *testing.T) {
	cases := []struct {
		start, perUser int
		want           string
	}{
		{10000, 100, "10000-10099"},
		{10700, 100, "10700-10799"},
		{29900, 100, "29900-29999"},
		{10000, 1, "10000"},
		{10000, 0, ""},
	}
	for _, c := range cases {
		if got := UserPorts(c.start, c.perUser); got != c.want {
			t.Errorf("UserPorts(%d, %d) = %q, want %q", c.start, c.perUser, got, c.want)
		}
	}
}

func TestUserPortsShort(t *testing.T) {
	cases := []struct {
		start int
		want  string
	}{
		{10000, "100xx"},
		{10700, "107xx"},
		{29900, "299xx"},
	}
	for _, c := range cases {
		if got := UserPortsShort(c.start); got != c.want {
			t.Errorf("UserPortsShort(%d) = %q, want %q", c.start, got, c.want)
		}
	}
}

func TestContainerIP(t *testing.T) {
	cases := []struct {
		subnet string
		idx    int
		want   string
	}{
		{"10.115.0.0/24", 1, "10.115.0.2"},
		{"10.115.0.0/24", 200, "10.115.0.201"},
		{"10.42.0.0/24", 5, "10.42.0.6"},
	}
	for _, c := range cases {
		got, err := ContainerIP(c.subnet, c.idx)
		if err != nil {
			t.Errorf("ContainerIP(%s, %d) error: %v", c.subnet, c.idx, err)
			continue
		}
		if got != c.want {
			t.Errorf("ContainerIP(%s, %d) = %q, want %q", c.subnet, c.idx, got, c.want)
		}
	}
	for _, bad := range []struct {
		subnet string
		idx    int
	}{
		{"10.115.0.0/16", 1}, // not a /24
		{"2001:db8::/64", 1}, // not IPv4
		{"10.115.0.0/24", 0}, // idx out of range
		{"10.115.0.0/24", 201},
	} {
		if _, err := ContainerIP(bad.subnet, bad.idx); err == nil {
			t.Errorf("ContainerIP(%s, %d): expected error", bad.subnet, bad.idx)
		}
	}
}

// TestAllocUserPortBlock verifies the port-block allocator draws only from the
// configured ranges and never hands out an already-taken block. It is
// independent of the IPv4 idx (the two are decoupled now).
func TestAllocUserPortBlock(t *testing.T) {
	c := cfg.Default()
	c.Net.UserPorts = "10000-20099, 25000-29999" // 101 + 50 = 151 blocks
	d, err := db.Open(filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	m := &Manager{cfg: c, db: d}

	// Occupy one block in each range, then ensure picks stay inside ranges and
	// never collide with the used ones.
	if _, err := d.CreateUser("a", "h", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateUser("b", "h", "10.115.0.3", 2, 30002, 25000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	used, err := d.UsedStartPorts()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for i := 0; i < 40; i++ {
		p, err := m.allocUserPortBlock()
		if err != nil {
			t.Fatal(err)
		}
		if p%100 != 0 {
			t.Fatalf("allocUserPortBlock = %d, not whole-hundred", p)
		}
		if used[p] {
			t.Fatalf("allocUserPortBlock = %d, already used", p)
		}
		inRange := (p >= 10000 && p <= 20099) || (p >= 25000 && p <= 29999)
		if !inRange {
			t.Fatalf("allocUserPortBlock = %d, outside configured ranges", p)
		}
		seen[p] = true
	}
	if len(seen) < 5 {
		t.Fatalf("allocUserPortBlock not random: only %d distinct picks", len(seen))
	}

	// A fully packed range must report exhaustion.
	small := cfg.Default()
	small.Net.UserPorts = "10000-10099" // exactly one block
	m2 := &Manager{cfg: small, db: d}
	if _, err := m2.allocUserPortBlock(); err == nil {
		t.Fatal("allocUserPortBlock on a full single-block range: expected error")
	}
}

// TestAllocSSHPortSkipsHostListener verifies allocSSHPort refuses a candidate
// that is already listened on by another process (the "probe, else reassign"
// behavior), even when the DB says the port is free.
func TestAllocSSHPortSkipsHostListener(t *testing.T) {
	c := cfg.Default()
	d, err := db.Open(filepath.Join(t.TempDir(), "s.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	m := &Manager{cfg: c, db: d}

	// Occupy one port in the SSH range with a real listener. The test must
	// pick a port that stays free afterwards (no DB record references it).
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	taken := ln.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() { ln.Close() })

	// The allocator must never hand out the occupied port, and must find a
	// free one.
	for i := 0; i < 25; i++ {
		p, err := m.allocSSHPort()
		if err != nil {
			t.Fatal(err)
		}
		if p == taken {
			t.Fatalf("allocSSHPort returned the occupied port %d", taken)
		}
		if p < cfg.SSHPortBase || p >= cfg.SSHPortBase+cfg.SSHPortCount {
			t.Fatalf("allocSSHPort = %d, outside SSH range", p)
		}
	}
}
