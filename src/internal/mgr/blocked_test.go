package mgr

import (
	"strings"
	"testing"
)

func TestParseBlockedList(t *testing.T) {
	cases := []struct {
		in        string
		wantValid []string
		wantBad   []int
	}{
		{"", nil, nil},
		{"  \n\n  ", nil, nil}, // whitespace-only lines ignored
		{"example.co.uk", []string{"example.co.uk"}, nil},     // plain domain
		{"  Example.CO.UK  ", []string{"example.co.uk"}, nil}, // trimmed + lowercased
		{"example.co.uk.", []string{"example.co.uk"}, nil},    // trailing dot stripped
		{"example.co.uk\nb.com", []string{"example.co.uk", "b.com"}, nil},
		{"example.co.uk\nexample.co.uk", []string{"example.co.uk"}, nil}, // deduped
		{"bad_domain", nil, []int{1}},                                    // underscore rejected
		{"-bad.com\nok.com\nbad..com", []string{"ok.com"}, []int{1, 3}},  // empty label reported by number
	}
	for _, c := range cases {
		valid, bad := ParseBlockedList(c.in)
		if !eqStrings(valid, c.wantValid) {
			t.Errorf("ParseBlockedList(%q) valid = %v, want %v", c.in, valid, c.wantValid)
		}
		if !eqInts(bad, c.wantBad) {
			t.Errorf("ParseBlockedList(%q) bad = %v, want %v", c.in, bad, c.wantBad)
		}
	}
}

func TestDomainBlockedMatching(t *testing.T) {
	m, d, _ := setupDomainTest(t, t.TempDir())
	if err := d.SetBlockedDomains([]string{"example.co.uk", "ads.io"}); err != nil {
		t.Fatal(err)
	}
	blocked := []string{
		"example.co.uk",       // exact
		"a.example.co.uk",     // one label
		"a.b.c.example.co.uk", // deep subdomain
		"www.ads.io",
	}
	allowed := []string{
		"forexample.co.uk", // shared suffix, not a subdomain
		"example.com",      // different TLD
		"co.uk",            // a parent is fine when not listed
		"notexample.co.uk",
	}
	for _, s := range blocked {
		if b, err := m.DomainBlocked(s); err != nil || b == "" {
			t.Errorf("DomainBlocked(%q) = %q, %v; want blocked", s, b, err)
		}
	}
	for _, s := range allowed {
		if b, err := m.DomainBlocked(s); err != nil || b != "" {
			t.Errorf("DomainBlocked(%q) = %q, %v; want allowed", s, b, err)
		}
	}
	// The matching entry is reported back.
	if b, err := m.DomainBlocked("a.b.example.co.uk"); err != nil || b != "example.co.uk" {
		t.Errorf("DomainBlocked(sub) = %q, %v; want example.co.uk", b, err)
	}
}

func TestAddDomainRefusesBlocked(t *testing.T) {
	m, d, name := setupDomainTest(t, t.TempDir())
	if err := d.SetBlockedDomains([]string{"example.co.uk"}); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"example.co.uk", "a.example.co.uk"} {
		if err := m.AddDomain(name, s, false); err == nil || !strings.Contains(err.Error(), "blocked") {
			t.Errorf("AddDomain(%q) err = %v, want blocked-domain error", s, err)
		}
	}
	if _, err := d.GetDomainByDomain("example.co.uk"); err == nil {
		t.Fatal("blocked domain was inserted into the DB")
	}
	// An unrelated domain still works.
	if err := m.AddDomain(name, "ok.example.com", false); err != nil {
		t.Fatalf("AddDomain(unblocked) = %v", err)
	}
	// A domain bound before the list was set stays bound — the blocklist only
	// guards the add path, never touches existing rows.
	if err := d.SetBlockedDomains([]string{"ok.example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetDomainByDomain("ok.example.com"); err != nil {
		t.Fatalf("existing domain lost after blocking it: %v", err)
	}
}

func TestNormalizeDomainRules(t *testing.T) {
	invalid := []string{
		"bad..com",            // consecutive dots
		"a.b..com",            // consecutive dots
		".a.b.com",            // leading dot
		"a-.b.com",            // hyphen glued to a dot
		"-a.com",              // leading hyphen
		"a.b.com-",            // trailing hyphen
		"1.2.3.4",             // dotted-quad IPv4 (ends with a digit)
		"https://example.com", // URL pasted in — rejected, not silently rewritten
		"my_domain.com",       // underscore
		"example",             // single word, no dot
		"localhost",           // single word, no dot
		"a..b",                // consecutive dots
	}
	for _, bad := range invalid {
		if _, err := normalizeDomain(bad); err == nil {
			t.Errorf("normalizeDomain(%q): expected error", bad)
		}
	}
	valid := []string{
		"example.com",
		"a.b.example.co.uk",
		"a1b.example.com",      // digits mid-label
		"a-b.example.com",      // hyphen mid-label
		"a--b.example.com",     // consecutive hyphens are legal per RFC 1035
		"xn--p1ai.example.com", // punycode prefix
		"x.com",                // single-letter label
		"1.com",                // leading-digit label is fine now
		"a-1.b.com",            // digit next to hyphen is fine
		"1.2.example.com",      // numeric subdomain labels
		"example.a",            // no TLD validation
	}
	for _, ok := range valid {
		if _, err := normalizeDomain(ok); err != nil {
			t.Errorf("normalizeDomain(%q) = %v, want nil", ok, err)
		}
	}
	// Trailing dots — one or many — are normalized away.
	for _, in := range []string{"example.com.", "example.com..", "example.com..."} {
		if got, err := normalizeDomain(in); err != nil || got != "example.com" {
			t.Errorf("normalizeDomain(%q) = %q, %v; want example.com, nil", in, got, err)
		}
	}
	// Length limits: a 64-char label and a >253 total are both rejected.
	longLabel := strings.Repeat("a", 64) + ".com"
	if _, err := normalizeDomain(longLabel); err == nil {
		t.Error("normalizeDomain(64-char label): expected error")
	}
	longDomain := strings.Repeat("a.", 130) + "com" // 261 chars
	if _, err := normalizeDomain(longDomain); err == nil {
		t.Error("normalizeDomain(>253 chars): expected error")
	}
	// The add path shares the same rules.
	m, _, name := setupDomainTest(t, t.TempDir())
	for _, bad := range []string{"bad..com", ".a.b.com", "1.2.3.4", "https://example.com"} {
		if err := m.AddDomain(name, bad, false); err == nil {
			t.Errorf("AddDomain(%q) should fail", bad)
		}
	}
}

func eqStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func eqInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
