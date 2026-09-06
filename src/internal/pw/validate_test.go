package pw

import "testing"

func TestValidate(t *testing.T) {
	cases := []struct {
		pass string
		want string
	}{
		{"", RejectTooShort},
		{"abcdefghi", RejectTooShort},   // 9 chars
		{"abc1234567", ""},              // 10 chars, letters + digits
		{"abcdefghij", RejectWeak},      // letters only
		{"1234567890", RejectWeak},      // digits only
		{"Ab1dEfGhIj", ""},              // mixed case + digits
		{"a1b2c3d4e5f6", ""},            // long enough, letters + digits
		{"Ab1", RejectTooShort},         // short but has both classes
	}
	for _, c := range cases {
		if got := Validate(c.pass); got != c.want {
			t.Errorf("Validate(%q) = %q, want %q", c.pass, got, c.want)
		}
	}
}
