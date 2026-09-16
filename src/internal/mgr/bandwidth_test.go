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

// TestBandwidthNextReset pins the date the quota label shows. The next reset
// is the configured day of the month AFTER the current PERIOD started — not
// "the configured day next month" — which is exactly the distinction that made
// the label wrong before: with resetDay=22 on the 16th the period ends on the
// 22nd of THIS month.
func TestBandwidthNextReset(t *testing.T) {
	day := func(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }
	cases := []struct {
		name    string
		t       time.Time
		reset   int
		want    time.Time
		thisMon bool // does the reset land in the same calendar month as t?
	}{
		// The reported case: today is the 16th, the reset day is the 22nd, so
		// the period in force started on the 22nd of LAST month and ends on the
		// 22nd of THIS one.
		{"16th with reset 22 -> this month", day(2026, 9, 16), 22, day(2026, 9, 22), true},
		// Once that day has passed, the next one is next month's.
		{"23rd with reset 22 -> next month", day(2026, 9, 23), 22, day(2026, 10, 22), false},
		{"on the reset day -> next month", day(2026, 9, 22), 22, day(2026, 10, 22), false},
		// Reset day 1: mid-month the next reset is always the 1st of next month.
		{"16th with reset 1 -> next month", day(2026, 9, 16), 1, day(2026, 10, 1), false},
		// ... except on the last day of a month, where it is the very next day
		// (still a different calendar month, so the label says "next month").
		{"last day with reset 1 -> September", day(2026, 8, 31), 1, day(2026, 9, 1), false},
		{"just after new year with reset 1", day(2026, 1, 1), 1, day(2026, 2, 1), false},
		// Year rollover, both directions.
		{"before reset in January", day(2026, 1, 10), 15, day(2026, 1, 15), true},
		{"December past the reset day", day(2026, 12, 20), 15, day(2027, 1, 15), false},
		// February is why the day is capped at 28.
		{"Feb with reset 28", day(2026, 2, 27), 28, day(2026, 2, 28), true},
		{"Feb with reset 28 past", day(2026, 2, 28), 28, day(2026, 3, 28), false},
		// Out of range clamps to 1, exactly like BandwidthPeriod.
		{"out of range clamps to 1", day(2026, 9, 16), 0, day(2026, 10, 1), false},
		{"out of range clamps to 1 (high)", day(2026, 9, 16), 40, day(2026, 10, 1), false},
	}
	for _, c := range cases {
		got := BandwidthNextReset(c.t, c.reset)
		if !got.Equal(c.want) {
			t.Errorf("%s: BandwidthNextReset(%s, %d) = %s, want %s",
				c.name, c.t.Format("2006-01-02"), c.reset, got.Format("2006-01-02"), c.want.Format("2006-01-02"))
		}
		if wantThisMon := got.Month() == c.t.Month() && got.Year() == c.t.Year(); wantThisMon != c.thisMon {
			t.Errorf("%s: same-month flag = %v, want %v", c.name, wantThisMon, c.thisMon)
		}
	}
}

// TestBandwidthNextResetAgreesWithPeriod keeps the two helpers from drifting:
// the period key flips exactly AT the returned reset moment, so a label built
// from one and a rollover driven by the other can never disagree about when
// the counters reset.
func TestBandwidthNextResetAgreesWithPeriod(t *testing.T) {
	for _, reset := range []int{1, 5, 15, 22, 28} {
		for d := 1; d <= 28; d++ {
			now := time.Date(2026, 9, d, 12, 0, 0, 0, time.UTC)
			next := BandwidthNextReset(now, reset)
			if !next.After(now) {
				t.Errorf("reset=%d day=%d: next reset %s is not in the future of %s",
					reset, d, next.Format("2006-01-02"), now.Format("2006-01-02"))
			}
			// Just before the boundary the OLD period is still in force; at the
			// boundary itself the key has already advanced.
			before := BandwidthPeriod(next.Add(-time.Second), reset)
			at := BandwidthPeriod(next, reset)
			if before == at {
				t.Errorf("reset=%d day=%d: period did not advance at %s (%s -> %s)",
					reset, d, next.Format("2006-01-02"), before, at)
			}
			// And the period in force right now must be the one this reset ends.
			if got := BandwidthPeriod(now, reset); got != before {
				t.Errorf("reset=%d day=%d: period now = %s, but the reset at %s ends %s",
					reset, d, got, next.Format("2006-01-02"), before)
			}
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
