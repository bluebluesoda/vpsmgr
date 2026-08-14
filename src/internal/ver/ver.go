package ver

// Version is the vpsmgr release version. CI overrides it at build time via
// -ldflags "-X vpsmgr/internal/ver.Version=...". Dev builds default to 0.3.0.
var Version = "0.3.0"

// MinAdoptable is the oldest release whose config/db format the current binary
// can adopt. v0.3 makes breaking changes, so only a config that already
// records an origin >= 0.3.0 (or was written by this release) may be adopted;
// anything from 0.2.x or older is rejected until a migration exists.
const MinAdoptable = "0.3.0"

// Blocked reports whether a config/db that records its origin as version
// "from" is too old to be adopted by the current binary. An empty or
// unparseable origin (configs predating the metadata fields, or hand-edited)
// is treated as too old: an unknown origin must never silently adopt a
// breaking release.
func Blocked(from string) bool {
	if from == "" {
		return true
	}
	return SemVerLT(from, MinAdoptable)
}

// SemVerLT reports whether a < b for dotted numeric versions ("0.2.5" <
// "0.3.0"). Segments are compared numerically; a leading "v" and non-numeric
// segments are tolerated (treated as 0), so "v0.2.5" and "0.2.5" match.
func SemVerLT(a, b string) bool {
	aa := parts(a)
	bb := parts(b)
	for i := 0; i < 3; i++ {
		if aa[i] != bb[i] {
			return aa[i] < bb[i]
		}
	}
	return false
}

func parts(v string) [3]int {
	var out [3]int
	cur, idx := 0, 0
	for _, r := range v {
		if r == '.' {
			if idx < 3 {
				out[idx] = cur
			}
			cur, idx = 0, idx+1
			continue
		}
		if r < '0' || r > '9' {
			cur = 0
			continue
		}
		cur = cur*10 + int(r-'0')
	}
	if idx < 3 {
		out[idx] = cur
	}
	return out
}
