package mgr

import (
	"regexp"
	"testing"
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
