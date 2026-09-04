package mgr

import (
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	valid := []string{
		"a", "alice", "aliceb",
		// Hyphens are allowed, but only between a leading and a trailing letter.
		"al-ice", "my-box", "a-b", "a1-b2-c", "x--y", "a1b",
	}
	invalid := []string{
		// Must end with a letter, so digit- and hyphen-terminated names are out.
		"", "1alice", "1", "Alice", "ALICE", "alice_", "alice.",
		"-alice", "alice-", "a-", "-a", "al-ice-", "al ice", "alice@x",
		"al.bob", "alice1", "a1", "vps12345678", "a1b2", strings.Repeat("a", 32), strings.Repeat("a", 31) + "-",
	}
	for _, n := range valid {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", n, err)
		}
	}
	for _, n := range invalid {
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", n)
		}
	}
}

// TestValidateExistingName verifies the looser check used by operations that
// act on an EXISTING user (del/quota/passwd): it must accept every historically
// valid username — including the legacy digit-ending ones that the stricter
// creation rule now rejects — so old users are never locked out.
func TestValidateExistingName(t *testing.T) {
	// Everything creation now allows, plus the legacy digit-ending names and
	// even a trailing-hyphen legacy share, must remain operable.
	valid := []string{"a", "alice", "a1", "vps12345678", "al-ice", "my-box", "x-y"}
	invalid := []string{"", "1alice", "Alice", "al ice", "alice@x", ".a", strings.Repeat("a", 32)}
	for _, n := range valid {
		if err := ValidateExistingName(n); err != nil {
			t.Errorf("ValidateExistingName(%q) = %v, want nil", n, err)
		}
	}
	for _, n := range invalid {
		if err := ValidateExistingName(n); err == nil {
			t.Errorf("ValidateExistingName(%q) = nil, want error", n)
		}
	}
	// A new creation must reject a digit-ending name, but the same name stays
	// acceptable for existing-user operations.
	if err := ValidateName("vps12345678"); err == nil {
		t.Error("ValidateName should reject digit-ending names under the strict rule")
	}
	if err := ValidateExistingName("vps12345678"); err != nil {
		t.Errorf("ValidateExistingName should still accept the legacy name vps12345678: %v", err)
	}
}
