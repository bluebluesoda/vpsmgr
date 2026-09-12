package mgr

import (
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
)

func TestRandomHostname(t *testing.T) {
	re := regexp.MustCompile(`^vps-[0-9a-f]{8}$`)
	a, b := randomHostname(), randomHostname()
	if !re.MatchString(a) || !re.MatchString(b) {
		t.Errorf("unexpected hostnames: %q, %q", a, b)
	}
	if a == b {
		t.Errorf("expected distinct random hostnames, got %q twice", a)
	}
}

func TestFormatGB(t *testing.T) {
	cases := []struct {
		bytes uint64
		want  string
	}{
		{0, "0.0"},
		{1 << 30, "1.0"},
		{2<<30 + 1<<29, "2.5"},
		{100 * 1e6, "0.1"},
		{1536 * 1e6, "1.4"},
	}
	for _, c := range cases {
		if got := FormatGB(c.bytes); got != c.want {
			t.Errorf("FormatGB(%d) = %q, want %q", c.bytes, got, c.want)
		}
	}
}

func TestShouldThrottle(t *testing.T) {
	cases := []struct {
		used    uint64
		quotaGB int
		want    bool
	}{
		{0, 0, false},                 // unlimited
		{1 << 40, 0, false},           // unlimited even when huge
		{(100 << 30) - 1, 100, false}, // just under
		{100 << 30, 100, true},        // exactly at the quota
		{(100 << 30) + 1, 100, true},  // over
		{0, -1, false},                // negative quota treated as unlimited
	}
	for _, c := range cases {
		if got := shouldThrottle(c.used, c.quotaGB); got != c.want {
			t.Errorf("shouldThrottle(%d, %d) = %v, want %v", c.used, c.quotaGB, got, c.want)
		}
	}
}

func TestBandwidthPeriod(t *testing.T) {
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	cases := []struct {
		name  string
		t     time.Time
		reset int
		want  string
	}{
		{"day 1 any time is the current month", day(2026, 3, 31), 1, "2026-03"},
		{"before reset keeps previous month", day(2026, 3, 14), 15, "2026-02"},
		{"on reset rolls to current month", day(2026, 3, 15), 15, "2026-03"},
		{"year boundary", day(2026, 1, 10), 15, "2025-12"},
		{"day 28 early Feb", day(2026, 2, 27), 28, "2026-01"},
		{"day 28 on Feb 28", day(2026, 2, 28), 28, "2026-02"},
		{"out of range clamps to 1", day(2026, 3, 1), 0, "2026-03"},
		{"out of range clamps to 1 (high)", day(2026, 3, 31), 40, "2026-03"},
	}
	for _, c := range cases {
		if got := BandwidthPeriod(c.t, c.reset); got != c.want {
			t.Errorf("%s: BandwidthPeriod(%s, %d) = %q, want %q", c.name, c.t.Format("2006-01-02"), c.reset, got, c.want)
		}
	}
}

func TestParseBandwidthGB(t *testing.T) {
	for s, want := range map[string]int{"": 0, "0": 0, "100": 100, "  50 ": 50} {
		got, err := ParseBandwidthGB(s)
		if err != nil || got != want {
			t.Errorf("ParseBandwidthGB(%q) = %d, %v; want %d", s, got, err, want)
		}
	}
	for _, bad := range []string{"-1", "abc", "1.5", "100GB"} {
		if _, err := ParseBandwidthGB(bad); err == nil {
			t.Errorf("ParseBandwidthGB(%q): expected error", bad)
		}
	}
}

// TestResetBandwidthZeroesCounters verifies the manager reset zeroes a user's
// monthly traffic. The manager holds no throttle state before the first sampler
// pass, so no Incus call is made — the reset only touches the DB.
func TestResetBandwidthZeroesCounters(t *testing.T) {
	c := cfg.Default()
	c.Incus.Socket = "/nonexistent/vpsmgr-test.sock"
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	u, err := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateBandwidthQuota(u.ID, 1); err != nil {
		t.Fatal(err)
	}
	// Seed 2 GiB of traffic so the user is over the 1 GiB quota. The first call
	// establishes the baseline (0 used), the second adds the 2 GiB delta.
	if err := d.ApplyBandwidth(u.ID, "2026-08", 2<<30, 0, 111); err != nil {
		t.Fatal(err)
	}
	if err := d.ApplyBandwidth(u.ID, "2026-08", 4<<30, 0, 111); err != nil {
		t.Fatal(err)
	}
	m := New(c, d)

	if up, down := m.BandwidthFor(u.ID); up != 0 || down != 2<<30 {
		t.Fatalf("before reset: up=%d down=%d, want 2GiB/0", up, down)
	}
	if err := m.ResetBandwidth("alice"); err != nil {
		t.Fatal(err)
	}
	if up, down := m.BandwidthFor(u.ID); up != 0 || down != 0 {
		t.Errorf("after reset: up=%d down=%d, want 0/0", up, down)
	}
}
