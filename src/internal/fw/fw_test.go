package fw

import (
	"strings"
	"testing"

	"vpsmgr/internal/cfg"
)

// TestMainContentStartsWithDeleteTable verifies the atomic-reload contract:
// the generated config begins with `delete table` (after the comment banner),
// so a single `nft -f` applies the whole ruleset as one batch instead of
// deleting and re-adding in two separate commands.
func TestMainContentStartsWithDeleteTable(t *testing.T) {
	c := cfg.Default()
	content := mainContent(c)
	if !strings.HasPrefix(content, cfg.GeneratedBanner) {
		t.Fatalf("mainContent missing the managed-by banner:\n%s", content)
	}
	if !strings.Contains(content, "delete table inet vpsmgr\n") {
		t.Fatalf("mainContent does not start with delete table:\n%s", content)
	}
	if !strings.Contains(content, `include "/etc/vpsmgr/nftables.d/*.nft"`) {
		t.Fatalf("mainContent missing the per-user include:\n%s", content)
	}
}

// TestMainContentBlocksPort25: port 25 (SMTP) must be dropped for ALL
// forwarded traffic, both directions, TCP and UDP — a permanent anti-spam
// rule that only a full uninstall removes.
func TestMainContentBlocksPort25(t *testing.T) {
	c := cfg.Default()
	content := mainContent(c)
	for _, want := range []string{"tcp dport 25 drop", "tcp sport 25 drop", "udp dport 25 drop", "udp sport 25 drop"} {
		if !strings.Contains(content, want) {
			t.Errorf("mainContent missing %q:\n%s", want, content)
		}
	}
}
