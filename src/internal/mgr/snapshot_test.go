package mgr

import (
	"strings"
	"testing"
)

func TestValidSnapName(t *testing.T) {
	valid := []string{
		"snap-20260820-153000",
		"a",
		"a.b_c-d",
		"snap1",
	}
	invalid := []string{
		"", "-a", ".a", "../evil", "a/b", "a b", "a+b", "a\\b",
		strings.Repeat("x", 65), // Incus name length limit
	}
	for _, n := range valid {
		if !ValidSnapName(n) {
			t.Errorf("ValidSnapName(%q) = false, want true", n)
		}
	}
	for _, n := range invalid {
		if ValidSnapName(n) {
			t.Errorf("ValidSnapName(%q) = true, want false", n)
		}
	}
}

func TestSnapNameFormat(t *testing.T) {
	n := snapName()
	if !strings.HasPrefix(n, "snap-") {
		t.Errorf("snapName() = %q, want snap- prefix", n)
	}
	if !ValidSnapName(n) {
		t.Errorf("snapName() = %q does not pass ValidSnapName", n)
	}
	// snap-YYYYMMDD-HHMMSS-XXXX = 8+1+6+1+4 = 20 chars after the prefix.
	if len(n) != len("snap-")+20 {
		t.Errorf("snapName() = %q, want snap-<20 chars>", n)
	}
	// Two calls in the same second must differ (random suffix).
	if snapName() == snapName() {
		t.Error("snapName() returned the same value twice; concurrent creates would collide")
	}
}

func TestFormatBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{12 * 1024 * 1024, "12.0 MiB"},
		{1024 * 1024 * 1024, "1.0 GiB"},
	}
	for _, c := range cases {
		if got := FormatBytes(c.in); got != c.want {
			t.Errorf("FormatBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
