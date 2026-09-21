package mgr

import (
	"net"
	"path/filepath"
	"strings"
	"testing"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
)

// extraTestManager is a prefix-mode manager with the standard base /64 and the
// given extra prefix, holding no accounts yet.
func extraTestManager(t *testing.T, extra string) *Manager {
	t.Helper()
	c := cfg.Default()
	c.Net.IPv6Subnet = "2602:fada:6::/64"
	c.Net.IPv6ExtraPrefix = extra
	d, err := db.Open(filepath.Join(t.TempDir(), "extra.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return &Manager{cfg: c, db: d}
}

// addExtraUser creates an account carrying the given whole /64.
func addExtraUser(t *testing.T, m *Manager, name, block string, idx int) *db.User {
	t.Helper()
	u, err := m.db.CreateUserFull(name, "h", "10.42.0."+string(rune('2'+idx)), idx, 30000+idx, 10000+idx*100,
		1, 1024, 10, 0, db.StatusReady, "", int64(idx), block, "")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// A /60 holds 16 /64s, addressed by an index in the bits between the prefix
// length and the /64 boundary — the mapping every allocation depends on.
func TestExtraBlockLayout(t *testing.T) {
	m := extraTestManager(t, "2602:fada:6::/60")
	p, err := m.ExtraPrefixNetwork()
	if err != nil || p == nil {
		t.Fatalf("extra prefix: %v %v", p, err)
	}
	if got := extraBlockCount(p); got != 16 {
		t.Errorf("extraBlockCount(/60) = %d, want 16", got)
	}
	for idx, want := range map[uint64]string{
		0:  "2602:fada:6::/64",
		1:  "2602:fada:6:1::/64",
		15: "2602:fada:6:f::/64",
	} {
		if got := extraBlockAt(p, idx); got != want {
			t.Errorf("extraBlockAt(/60, %d) = %s, want %s", idx, got, want)
		}
	}

	// The size follows the prefix length; a /64 or longer fits none.
	short, _ := (&cfg.Config{Net: cfg.NetCfg{IPv6ExtraPrefix: "2602:fada:6::/56"}}).IPv6ExtraPrefixNetwork()
	if got := extraBlockCount(short); got != 256 {
		t.Errorf("extraBlockCount(/56) = %d, want 256", got)
	}
	for _, s := range []string{"2602:fada:6::/64", "2602:fada:6::/80"} {
		q, _ := (&cfg.Config{Net: cfg.NetCfg{IPv6ExtraPrefix: s}}).IPv6ExtraPrefixNetwork()
		if got := extraBlockCount(q); got != 0 {
			t.Errorf("extraBlockCount(%s) = %d, want 0", s, got)
		}
	}
}

// Membership and the block walk have to agree for prefixes that are not a whole
// number of hextets, where it is easy to be off by one bit: a /63 holds exactly
// the two /64s that differ in the LAST bit of the fourth hextet, not in the
// third — the mistake that would make the pool look half-used.
func TestExtraBlockMembershipNonByteAligned(t *testing.T) {
	m := extraTestManager(t, "2001:db8:5678::/63")
	p, err := m.ExtraPrefixNetwork()
	if err != nil {
		t.Fatal(err)
	}
	if got := extraBlockAt(p, 1); got != "2001:db8:5678:1::/64" {
		t.Errorf("second /64 of a /63 = %s, want 2001:db8:5678:1::/64", got)
	}
	for _, c := range []struct {
		ip   string
		want bool
	}{
		{"2001:db8:5678::", true},
		{"2001:db8:5678:1::", true},
		{"2001:db8:5678:2::", false},
		{"2001:db8:5679::", false}, // a different /63 entirely
		{"2602:fada:6::", false},
	} {
		if _, bn, err := net.ParseCIDR(c.ip + "/64"); err != nil {
			t.Fatal(err)
		} else if got := p.Contains(bn.IP); got != c.want {
			t.Errorf("contains %s = %v, want %v", c.ip, got, c.want)
		}
	}
}

// The /64 this host uses must never be handed out. When the extra prefix
// contains it, it is reserved; when the prefix is somewhere else entirely,
// nothing is.
func TestExtraReservesTheHostsOwnBlock(t *testing.T) {
	m := extraTestManager(t, "2602:fada:6::/60")
	p, _ := m.ExtraPrefixNetwork()
	if res := m.reservedExtraBlocks(p); !res["2602:fada:6::/64"] {
		t.Errorf("the host's own /64 is not reserved: %v", res)
	}

	// A prefix that does not contain the base /64 reserves nothing of it — the
	// pool stays whole.
	m2 := extraTestManager(t, "2001:db8:1::/60")
	p2, _ := m2.ExtraPrefixNetwork()
	if res := m2.reservedExtraBlocks(p2); len(res) != 0 {
		t.Errorf("unrelated prefix reserved %v, want none", res)
	}
	total, reserved, _, free, err := m2.ExtraCapacity()
	if err != nil || total != 16 || reserved != 0 || free != 16 {
		t.Errorf("capacity = %d/%d/%d (err %v), want 16 total, 0 reserved, 16 free", total, reserved, free, err)
	}
}

// Allocation walks the prefix from the bottom, skipping the host's reserved
// /64, and reports exhaustion with "" (the caller then creates a container
// without a block rather than failing).
func TestPickExtraBlockSkipsReservedAndExhausts(t *testing.T) {
	// A /63 holds exactly two /64s; the first is the host's own, so exactly one
	// block is allocatable.
	m := extraTestManager(t, "2602:fada:6::/63")
	if got, _ := m.pickExtraBlock(); got != "2602:fada:6:1::/64" {
		t.Fatalf("pickExtraBlock = %q, want the second /64 (the first is the host's)", got)
	}
	addExtraUser(t, m, "alice", "2602:fada:6:1::/64", 1)
	if got, err := m.pickExtraBlock(); got != "" || err != nil {
		t.Errorf("pickExtraBlock after exhaustion = %q, %v; want \"\"", got, err)
	}

	// Sequential by construction: a /56 hands out the lowest free block each
	// time, so the admin page can read the used set off the prefix in order.
	m2 := extraTestManager(t, "2001:db8:2::/56")
	for i, want := range []string{"2001:db8:2::/64", "2001:db8:2:1::/64", "2001:db8:2:2::/64"} {
		got, err := m2.pickExtraBlock()
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("pick #%d = %q, want %q", i, got, want)
		}
		addExtraUser(t, m2, "u"+string(rune('a'+i)), got, i+1)
	}
}

// Capacity is what the panel shows and the checkbox greys out on.
func TestExtraCapacityCounts(t *testing.T) {
	m := extraTestManager(t, "2602:fada:6::/60")
	total, reserved, used, free, err := m.ExtraCapacity()
	if err != nil {
		t.Fatal(err)
	}
	if total != 16 || reserved < 1 || used != 0 || free != total-reserved {
		t.Errorf("capacity = total %d reserved %d used %d free %d", total, reserved, used, free)
	}
	addExtraUser(t, m, "alice", "2602:fada:6:1::/64", 1)
	if _, _, used, free2, _ := m.ExtraCapacity(); used != 1 || free2 != free-1 {
		t.Errorf("after one assignment: used %d free %d, want used 1 free %d", used, free2, free-1)
	}

	// Assignments that no longer belong to the configured prefix do not count
	// (the operator may have changed it), so the numbers stay consistent.
	m2 := extraTestManager(t, "2001:db8:3::/60")
	addExtraUser(t, m2, "alice", "2602:ffff::/64", 1)
	if _, _, used, _, _ := m2.ExtraCapacity(); used != 0 {
		t.Errorf("foreign block counted as used: %d", used)
	}
}

// The feature is off without a prefix, and never runs in pool mode.
func TestExtraDisabledByDefault(t *testing.T) {
	m := extraTestManager(t, "")
	if m.ExtraEnabled() {
		t.Error("ExtraEnabled with no configured prefix")
	}
	if b, err := m.pickExtraBlock(); b != "" || err != nil {
		t.Errorf("pickExtraBlock with no prefix = %q, %v", b, err)
	}
	if total, _, _, _, _ := m.ExtraCapacity(); total != 0 {
		t.Errorf("capacity with no prefix = %d, want 0", total)
	}

	c := cfg.Default()
	c.Net.IPv6Subnet = "2602:fada:6::/64"
	c.Net.IPv6Mode = cfg.IPv6ModePool
	c.Net.IPv6ExtraPrefix = "2602:fada:6::/60"
	d, err := db.Open(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	mp := &Manager{cfg: c, db: d}
	if mp.ExtraEnabled() {
		t.Error("ExtraEnabled in pool mode")
	}
	if b, _ := mp.pickExtraBlock(); b != "" {
		t.Errorf("pool mode handed out %q", b)
	}
}

// The guest must bind the whole /64 with its real length (so the prefix is
// on-link inside and the customer can carve sub-prefixes out of it), on both
// guest stacks, and must not let the parent-prefix flush delete it.
func TestIPv6ContainerScriptWithExtraBlock(t *testing.T) {
	m := ipv6TestManager(t, "2602:fada:6::/64", map[string]int64{"alice": legacyAlice})
	addr, err := m.IPv6Addr("alice")
	if err != nil {
		t.Fatal(err)
	}
	script, err := m.ipv6ContainerScript(addr, "2602:fada:9:1::/64")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Address=2602:fada:9:1::1/64",                                    // networkd: the /64 address
		"ipv6.addresses 2602:fada:6::2bd8:6c9:1/128,2602:fada:9:1::1/64", // RHEL (nmcli)
		`[ "$r" = 2602:fada:9:1::/64 ] && continue`,                      // flush guard
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	// A local route would claim every address in the block for the container
	// itself and make internal re-delegation impossible — the opposite of what
	// the block is for.
	if strings.Contains(script, "local 2602:fada:9:1::/64") {
		t.Errorf("the /64 must not be made a local route:\n%s", script)
	}

	// Without a block nothing of the above leaks into the script, and the
	// parent-prefix flush keeps its original shape (no skip guard).
	plain, err := m.ipv6ContainerScript(addr, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, unwanted := range []string{"EXTRAADDR", "2602:fada:9:1", `[ "$r" = `} {
		if strings.Contains(plain, unwanted) {
			t.Errorf("script without a block contains %q:\n%s", unwanted, plain)
		}
	}
}

// The whole /64 must be listed BEFORE the account's /112. Incus installs the
// list in order, each entry as a `via <primary>` route, and the kernel refuses a
// via-address route whose gateway is itself reached by one — which the /112
// route is ("RTNETLINK answers: No route to host"). Reordering this string
// silently costs every block container its /64, so pin it.
func TestExtraBlockRoutesOrder(t *testing.T) {
	const block112 = "2602:fada:6::/112"
	const block64 = "2602:fada:6:7::/64"
	if got, want := blockRoutes(block112, block64), block64+","+block112; got != want {
		t.Errorf("blockRoutes(%q, %q) = %q, want %q", block112, block64, got, want)
	}
	// Without a block the value is exactly what it has always been, and a
	// missing /112 (pool mode) must not leave a stray comma.
	if got := blockRoutes(block112, ""); got != block112 {
		t.Errorf("blockRoutes without a block = %q, want %q", got, block112)
	}
	if got := blockRoutes("", block64); got != block64 {
		t.Errorf("blockRoutes without a /112 = %q, want %q", got, block64)
	}
}

// Assignments are listed with their owner, in block order.
func TestExtraAssignments(t *testing.T) {
	m := extraTestManager(t, "2001:db8:4::/60")
	addExtraUser(t, m, "bob", "2001:db8:4:3::/64", 1)
	addExtraUser(t, m, "alice", "2001:db8:4:1::/64", 2)
	addExtraUser(t, m, "carol", "", 3) // no block: not listed
	got := m.ExtraAssignments()
	if len(got) != 2 {
		t.Fatalf("ExtraAssignments = %v, want 2 entries", got)
	}
	if got[0].Block != "2001:db8:4:1::/64" || got[0].User != "alice" ||
		got[1].Block != "2001:db8:4:3::/64" || got[1].User != "bob" {
		t.Errorf("ExtraAssignments = %v, want alice's then bob's", got)
	}
}
