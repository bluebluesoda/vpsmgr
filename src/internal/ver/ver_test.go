package ver

import "testing"

func TestSemVerLT(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.2.5", "0.3.0", true},
		{"0.2.0", "0.2.5", true},
		{"0.2.5", "0.2.4", false},
		{"0.3.0", "0.3.0", false},
		{"0.3.1", "0.3.0", false},
		{"0.9.9", "0.10.0", true},
		{"0.10.0", "0.9.9", false},
		{"1.0.0", "0.3.0", false},
		{"", "0.3.0", true},
		{"v0.2.5", "0.3.0", true},
		{"garbage", "0.3.0", true},
	}
	for _, c := range cases {
		if got := SemVerLT(c.a, c.b); got != c.want {
			t.Errorf("SemVerLT(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestBlocked(t *testing.T) {
	cases := []struct {
		from string
		want bool
	}{
		{"0.2.5", true},
		{"0.2.0", true},
		{"0.1.2", true},
		{"", true},
		{"0.3.0", false},
		{"0.3.1", false},
		{"v0.3.0", false},
	}
	for _, c := range cases {
		if got := Blocked(c.from); got != c.want {
			t.Errorf("Blocked(%q) = %v, want %v", c.from, got, c.want)
		}
	}
}
