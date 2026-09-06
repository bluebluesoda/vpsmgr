package pw

import "testing"

func TestValidate(t *testing.T) {
	cases := []struct {
		pass string
		want string
	}{
		{"", RejectTooShort},
		{"Ab1dEfGhI", RejectTooShort},  // 9 chars, otherwise complete
		{"Abcdefghij", RejectWeak},     // >= 10 but no digit
		{"ABCDEF1234", RejectWeak},     // no lowercase
		{"abcdef1234", RejectWeak},     // no uppercase
		{"1234567890", RejectWeak},     // no letters at all
		{"Abcdef1234", ""},             // 10 chars, upper + lower + digit
		{"a1b2c3d4e5F6", ""},           // long enough, all three classes
		{"A1b", RejectTooShort},        // short but has all classes
	}
	for _, c := range cases {
		if got := Validate(c.pass); got != c.want {
			t.Errorf("Validate(%q) = %q, want %q", c.pass, got, c.want)
		}
	}
}
