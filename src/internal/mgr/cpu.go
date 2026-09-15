package mgr

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// CPU quotas are stored as tenths of a core: 1..9 mean 0.1..0.9 cores, and
// 10, 20, ... mean whole cores. A fractional quota is enforced in Incus as
// `limits.cpu=1` plus a time allowance (`limits.cpu.allowance=<n>ms/100ms`),
// i.e. the container is pinned to a single core and may only use a slice of
// it; whole cores keep the plain `limits.cpu=<n>` limit.

var cpuIntRe = regexp.MustCompile(`^\d+$`)

// cpuFracRe matches one-decimal fractional cores in 0.1..0.9: "0.5" or ".5".
var cpuFracRe = regexp.MustCompile(`^0?\.([1-9])$`)

// ParseCPU parses a CPU quota string into tenths of a core. It accepts a whole
// number of cores (>= 1, e.g. "1", "4") or a one-decimal value in 0.1..0.9
// (e.g. "0.5", ".5"). Decimals above 1 are rejected: from one core up, quotas
// must be whole cores.
func ParseCPU(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("cpu: empty input")
	}
	if cpuIntRe.MatchString(s) {
		n, _ := strconv.Atoi(s)
		if n < 1 {
			return 0, errors.New("cpu must be at least 1 core, or a decimal in 0.1..0.9")
		}
		return n * 10, nil
	}
	if m := cpuFracRe.FindStringSubmatch(s); m != nil {
		t, _ := strconv.Atoi(m[1])
		return t, nil
	}
	return 0, errors.New("cpu must be a whole number of cores (>= 1) or a one-decimal value in 0.1..0.9")
}

// ValidateCPU checks a tenths-of-a-core quota: 1..9 (0.1..0.9 cores) or a
// whole number of cores (10, 20, ...).
func ValidateCPU(tenths int) error {
	if tenths < 1 {
		return errors.New("cpu must be at least 0.1")
	}
	if tenths < 10 {
		return nil
	}
	if tenths%10 != 0 {
		return errors.New("cpu must be a whole number of cores (>= 1) or a one-decimal value in 0.1..0.9")
	}
	return nil
}

// FormatCPU renders a tenths-of-a-core quota for display: 5 -> "0.5", 20 -> "2".
func FormatCPU(tenths int) string {
	if tenths%10 == 0 {
		return strconv.Itoa(tenths / 10)
	}
	return "0." + strconv.Itoa(tenths)
}
