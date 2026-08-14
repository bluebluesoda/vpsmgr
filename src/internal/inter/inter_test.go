package inter

import (
	"os"
	"strings"
	"testing"
)

// runConfirm feeds input to Confirm and returns the result.
func runConfirm(t *testing.T, input string, defTrue bool) bool {
	t.Helper()
	oldStdin := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	defer func() { os.Stdin = oldStdin }()
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	w.Close()
	rd = nil // reset the shared bufio.Reader
	got, err := Confirm("proceed", defTrue)
	r.Close()
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestConfirm(t *testing.T) {
	cases := []struct {
		input   string
		defTrue bool
		want    bool
	}{
		{"y\n", false, true},
		{"yes\n", false, true},
		{"n\n", true, false},
		{"no\n", true, false},
		{"\n", false, false}, // empty + default no
		{"\n", true, true},   // empty + default yes
		{"Y\n", false, true},
		{"N\n", true, false},
	}
	for _, c := range cases {
		if got := runConfirm(t, c.input, c.defTrue); got != c.want {
			t.Errorf("Confirm(%q, def=%v) = %v, want %v", strings.TrimSpace(c.input), c.defTrue, got, c.want)
		}
	}
}
