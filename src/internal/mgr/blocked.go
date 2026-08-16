package mgr

import "strings"

// DomainBlocked reports whether the already-normalized domain d is refused by
// the admin blocked-domains list: it is blocked when it exactly equals a
// blocked domain or is a subdomain of one (label boundary — "example.co.uk"
// matches "example.co.uk", "a.example.co.uk" and "a.b.c.example.co.uk" but
// not "forexample.co.uk"). Returns the matching blocked domain, or "" when
// the add is allowed. The list is read fresh from the DB on every call so a
// change made in the admin panel applies immediately.
func (m *Manager) DomainBlocked(d string) (string, error) {
	list, err := m.db.GetBlockedDomains()
	if err != nil {
		return "", err
	}
	for _, b := range list {
		if d == b || strings.HasSuffix(d, "."+b) {
			return b, nil
		}
	}
	return "", nil
}

// ParseBlockedList validates a textarea value (one domain per line, no
// wildcards) and returns the normalized, deduplicated entries plus the
// 1-based line numbers that were invalid. Empty and whitespace-only lines are
// ignored, not counted as errors. Invalid lines are skipped; the rest are
// usable as the new list (partial saves, reported by line number).
func ParseBlockedList(text string) (valid []string, badLines []int) {
	for i, ln := range strings.Split(text, "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		n, err := normalizeDomain(ln)
		if err != nil {
			badLines = append(badLines, i+1)
			continue
		}
		if !containsStr(valid, n) {
			valid = append(valid, n)
		}
	}
	return valid, badLines
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
