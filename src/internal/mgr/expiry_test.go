package mgr

import (
	"testing"
	"time"
)

func TestIsExpired(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		v    string
		want bool
	}{
		{"", false}, // permanent
		{"not-a-time", false},
		{now.Add(time.Hour).Format(time.RFC3339), false},
		{now.Add(-time.Minute).Format(time.RFC3339), true},
	}
	for _, c := range cases {
		if got := IsExpired(c.v, now); got != c.want {
			t.Errorf("IsExpired(%q) = %v, want %v", c.v, got, c.want)
		}
	}
}

func TestExpiryRemaining(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if got := ExpiryRemaining("", now); got != 0 {
		t.Errorf("permanent remaining = %v, want 0", got)
	}
	if got := ExpiryRemaining("nonsense", now); got != 0 {
		t.Errorf("unparseable remaining = %v, want 0", got)
	}
	if got := ExpiryRemaining(now.Add(90*time.Minute).Format(time.RFC3339), now); got != 90*time.Minute {
		t.Errorf("remaining = %v, want 90m", got)
	}
}

func TestExtendExpiry(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	d := 30 * 24 * time.Hour

	// Future deadline: extension stacks on top of it.
	future := now.Add(10 * 24 * time.Hour)
	if got, want := ExtendExpiry(future.Format(time.RFC3339), now, d), future.Add(d).Format(time.RFC3339); got != want {
		t.Errorf("extend future = %q, want %q", got, want)
	}
	// Already expired: the full duration counts from now.
	past := now.Add(-5 * 24 * time.Hour)
	if got, want := ExtendExpiry(past.Format(time.RFC3339), now, d), now.Add(d).Format(time.RFC3339); got != want {
		t.Errorf("extend past = %q, want %q", got, want)
	}
	// Permanent: extending creates a fresh deadline from now.
	if got, want := ExtendExpiry("", now, d), now.Add(d).Format(time.RFC3339); got != want {
		t.Errorf("extend permanent = %q, want %q", got, want)
	}
}

func TestExpiryFromDays(t *testing.T) {
	if got := ExpiryFromDays(0); got != "" {
		t.Errorf("ExpiryFromDays(0) = %q, want empty", got)
	}
	if got := ExpiryFromDays(-3); got != "" {
		t.Errorf("ExpiryFromDays(-3) = %q, want empty", got)
	}
	got := ExpiryFromDays(7)
	ts, err := time.Parse(time.RFC3339, got)
	if err != nil {
		t.Fatalf("ExpiryFromDays(7) = %q: %v", got, err)
	}
	if want := time.Now().UTC().AddDate(0, 0, 7); ts.Sub(want).Abs() > time.Minute {
		t.Errorf("ExpiryFromDays(7) = %v, want ~%v", ts, want)
	}
}
