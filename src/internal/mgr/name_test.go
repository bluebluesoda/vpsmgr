package mgr

import (
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	valid := []string{"a", "alice", "a1", "vps12345678"}
	invalid := []string{
		"", "1alice", "1", "Alice", "ALICE", "alice_", "alice.", "alice-",
		"-alice", "al-ice", "al ice", "alice@x", strings.Repeat("a", 32), "a-",
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
