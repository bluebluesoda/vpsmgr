package mgr

import "testing"

func TestParseCPU(t *testing.T) {
	valid := map[string]int{
		"0.1": 1, "0.2": 2, "0.5": 5, "0.9": 9, ".5": 5,
		"1": 10, "2": 20, "4": 40, "10": 100,
	}
	for in, want := range valid {
		got, err := ParseCPU(in)
		if err != nil {
			t.Errorf("ParseCPU(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseCPU(%q) = %d, want %d", in, got, want)
		}
	}
	invalid := []string{"", "  ", "0", "0.0", "0.05", "0.11", "1.0", "1.1", "1.5", "2.0", "-1", "-0.5", "abc", "0x", "1e2"}
	for _, in := range invalid {
		if _, err := ParseCPU(in); err == nil {
			t.Errorf("ParseCPU(%q) accepted, want error", in)
		}
	}
}

func TestValidateCPU(t *testing.T) {
	valid := []int{1, 5, 9, 10, 20, 40, 100}
	for _, v := range valid {
		if err := ValidateCPU(v); err != nil {
			t.Errorf("ValidateCPU(%d) error: %v", v, err)
		}
	}
	invalid := []int{0, -1, 11, 15, 21, 99}
	for _, v := range invalid {
		if err := ValidateCPU(v); err == nil {
			t.Errorf("ValidateCPU(%d) accepted, want error", v)
		}
	}
}

func TestFormatCPU(t *testing.T) {
	cases := map[int]string{1: "0.1", 5: "0.5", 9: "0.9", 10: "1", 20: "2", 40: "4", 100: "10"}
	for in, want := range cases {
		if got := FormatCPU(in); got != want {
			t.Errorf("FormatCPU(%d) = %q, want %q", in, got, want)
		}
	}
}
