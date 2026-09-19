package mgr

import (
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
)

// legacyAlice is the block index the old username-derived scheme gave "alice"
// (sha256("alice")[:4] = 0x2bd806c9): the documented example, and what the v19
// migration seeds for an existing account of that name. Tests that assert an
// address value use it, so they keep asserting the address that scheme has
// always produced.
const legacyAlice = 0x2bd806c9

// ipv6TestManager returns a manager with prefix-mode IPv6 on subnet and a
// database holding one account per given name, each with the block index given.
func ipv6TestManager(t *testing.T, subnet string, users map[string]int64) *Manager {
	t.Helper()
	c := cfg.Default()
	c.Net.IPv6Subnet = subnet
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	i := 0
	for name, index := range users {
		i++
		if _, err := d.CreateUserFull(name, "h", fmt.Sprintf("10.42.0.%d", i+1), i, 30000+i, 10000+i*100,
			1, 1024, 10, 0, db.StatusReady, "", index, ""); err != nil {
			t.Fatal(err)
		}
	}
	return &Manager{cfg: c, db: d}
}

// The block and the primary address come from the index stored with the
// account, not from its name — which is the point of storing it: the suffix of
// an address must not spell out who owns it.
func TestIPv6AddrFollowsStoredIndex(t *testing.T) {
	m := ipv6TestManager(t, "2602:fada:6::/64", map[string]int64{"alice": legacyAlice})
	addr, err := m.IPv6Addr("alice")
	if err != nil {
		t.Fatal(err)
	}
	// An account seeded with the legacy value keeps the address it has always
	// had: 2602:fada:6::<hash>:1.
	if want := "2602:fada:6::2bd8:6c9:1"; addr != want {
		t.Errorf("IPv6Addr(alice) = %q, want %q", addr, want)
	}

	// The same name on another host, with a different stored index, gets a
	// different address — no cross-host fingerprint.
	other := ipv6TestManager(t, "2602:fada:6::/64", map[string]int64{"alice": 1})
	otherAddr, err := other.IPv6Addr("alice")
	if err != nil {
		t.Fatal(err)
	}
	if otherAddr == addr {
		t.Errorf("two hosts gave %q the same address for the same name", otherAddr)
	}
}

// An address must always fall inside the configured subnet, for any supported
// prefix length (/64 down to /48, plus /80 provider slices). The 32-bit index
// and the 16 host bits only touch the low 48 bits, which are host bits for
// every prefix <= /80.
func TestIPv6AddrWithinSubnet(t *testing.T) {
	for _, sub := range []string{"2602:fada:6::/48", "2602:fada:6::/56", "2602:fada:6::/60", "2602:fada:6::/64", "2406:da14:1dd2:a807:753a::/80"} {
		m := ipv6TestManager(t, sub, map[string]int64{"alice": legacyAlice})
		addr, err := m.IPv6Addr("alice")
		if err != nil {
			t.Fatalf("%s: %v", sub, err)
		}
		_, ipnet, err := net.ParseCIDR(sub)
		if err != nil {
			t.Fatal(err)
		}
		if !ipnet.Contains(net.ParseIP(addr)) {
			t.Errorf("%s: addr %s not inside subnet", sub, addr)
		}
	}
}

// A /80 provider slice must keep ALL prefix bits (e.g. the 753a hextet) — only
// the low 48 bits may come from the index and the host space.
func TestIPv6Addr80(t *testing.T) {
	m := ipv6TestManager(t, "2406:da14:1dd2:a807:753a::/80", map[string]int64{"alice": legacyAlice})
	addr, err := m.IPv6Addr("alice")
	if err != nil {
		t.Fatal(err)
	}
	if want := "2406:da14:1dd2:a807:753a:2bd8:6c9:1"; addr != want {
		t.Errorf("IPv6Addr(alice) = %q, want %q", addr, want)
	}
}

// A block is a /112 with its host bits clear, inside the configured subnet, and
// distinct indexes are distinct blocks.
func TestIPv6Block(t *testing.T) {
	m := ipv6TestManager(t, "2602:fada:6::/64", map[string]int64{"alice": legacyAlice})
	b, err := m.IPv6Block("alice")
	if err != nil {
		t.Fatal(err)
	}
	if b == nil {
		t.Fatal("nil block")
	}
	ones, _ := b.Mask.Size()
	if ones != 112 {
		t.Errorf("block mask = %d, want 112", ones)
	}
	if b.IP.To16()[14] != 0 || b.IP.To16()[15] != 0 {
		t.Errorf("block host bits not zero: %s", b.IP)
	}
	if got := addHostOffset(b.IP, 1).String(); got != "2602:fada:6::2bd8:6c9:1" {
		t.Errorf("block+1 = %s, want 2602:fada:6::2bd8:6c9:1", got)
	}
	_, ipnet, err := net.ParseCIDR("2602:fada:6::/64")
	if err != nil {
		t.Fatal(err)
	}
	if !ipnet.Contains(b.IP) {
		t.Errorf("block %s outside subnet", b.String())
	}
	other, err := m.ipv6BlockIdx(1)
	if err != nil {
		t.Fatal(err)
	}
	if other.IP.String() == b.IP.String() {
		t.Errorf("two indexes share block %s", b)
	}
}

// pickIPv6Index must never hand out an index another account holds, and never
// one whose block contains the bridge gateway (index 0 is that block, which is
// also why 0 means "no index").
func TestPickIPv6Index(t *testing.T) {
	m := ipv6TestManager(t, "2602:fada:6::/64", map[string]int64{"alice": 0, "bob": 7})
	_, ipnet, err := net.ParseCIDR("2602:fada:6::/64")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 32; i++ {
		got, err := m.pickIPv6Index()
		if err != nil {
			t.Fatal(err)
		}
		if got == 0 || got == 7 {
			t.Fatalf("pickIPv6Index returned %d, which is taken or the gateway's block", got)
		}
		block, err := m.ipv6BlockIdx(got)
		if err != nil {
			t.Fatal(err)
		}
		if !ipnet.Contains(block.IP) {
			t.Fatalf("picked block %s is outside the subnet", block)
		}
	}
}

// The bridge gateway (net+1, net+2, ...) must stay inside the prefix, for both
// /64 and /80 provider slices — this is the arithmetic behind avoiding a host
// or router that already holds ::1.
func TestAddHostOffset(t *testing.T) {
	cases := []struct{ subnet, want1, want2 string }{
		{"2602:fada:6::/64", "2602:fada:6::1", "2602:fada:6::2"},
		{"2406:da14:1dd2:a807:753a::/80", "2406:da14:1dd2:a807:753a::1", "2406:da14:1dd2:a807:753a::2"},
	}
	for _, c := range cases {
		_, n, err := net.ParseCIDR(c.subnet)
		if err != nil {
			t.Fatal(err)
		}
		got1 := addHostOffset(n.IP, 1).String()
		got2 := addHostOffset(n.IP, 2).String()
		if got1 != c.want1 || got2 != c.want2 {
			t.Errorf("%s: net+1=%q (want %q), net+2=%q (want %q)", c.subnet, got1, c.want1, got2, c.want2)
		}
	}
}

// The bridge is always >= /64: Incus's dnsmasq rejects non-/64 networks, and
// all container addresses live in the first /64 of the prefix.
func TestBridgePrefixLen(t *testing.T) {
	cases := []struct{ ones, want int }{
		{48, 64}, {56, 64}, {60, 64}, {64, 64}, {80, 80},
	}
	for _, c := range cases {
		if got := bridgePrefixLen(c.ones); got != c.want {
			t.Errorf("bridgePrefixLen(%d) = %d, want %d", c.ones, got, c.want)
		}
	}
}

// The generated container script must: keep the parent prefix off-link, forbid
// SLAAC, statically bind the account's /128, turn DHCPv6 off, strip the mangled
// residue buggy older versions wrote, and flush stale on-link routes.
func TestIPv6ContainerScript(t *testing.T) {
	m := ipv6TestManager(t, "2602:fada:6::/64", map[string]int64{"alice": legacyAlice})
	addr, err := m.IPv6Addr("alice") // the account's stored block index
	if err != nil {
		t.Fatal(err)
	}
	script, err := m.ipv6ContainerScript(addr)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"2602:fada:6::2bd8:6c9:1",             // the account's primary address
		"UseOnLinkPrefix=false",               // peers via the host, not L2
		"UseRoutePrefix=false",                // parent prefix never a route
		"UseAutonomousPrefix=false",           // no SLAAC address outside the /112
		"DHCPv6Client=no",                     // RA Managed flag must not start DHCPv6
		"Address=2602:fada:6::2bd8:6c9:1/128", // static bind, DHCPv6-independent
		"DHCP=ipv4",                           // DHCPv6 off
		"s/^DHCP=true$/DHCP=ipv4/",            // flips the baked DHCP=true
		"n\\[IPv6AcceptRA\\]",                 // heals mangled old configs (awk regex)
		"2602:fada:6*",                        // stale on-link route flush
		"ip -6 route flush cache",
		"ipv6.method manual",                         // RHEL: NM owns the IPv6 stack
		"ipv6.addresses 2602:fada:6::2bd8:6c9:1/128", // the account's /128
		"ipv6.gateway fe80::1",                       // bridge's fixed link-local gateway
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	// The RA options must appear in a real [IPv6AcceptRA] section, not a
	// mangled single line (the bug that made the old fix a no-op): backslash-n
	// escapes degraded to literal 'n', e.g. 'n[IPv6AcceptRA]nUseOnLinkPrefix'.
	if strings.Contains(script, "n[IPv6AcceptRA]nUseOnLinkPrefix") {
		t.Errorf("script contains mangled residue:\n%s", script)
	}
}

// With IPv6 disabled the whole flow is a no-op — it must not reach for a
// container (or the database) at all.
func TestConfigureContainerIPv6Disabled(t *testing.T) {
	c := cfg.Default() // IPv6 disabled by default
	m := &Manager{cfg: c}
	if err := m.ConfigureContainerIPv6("alice", ""); err != nil {
		t.Errorf("expected a no-op when IPv6 is disabled, got %v", err)
	}
}

// An account with no address (V4-only) yields no script in either mode.
func TestIPv6ContainerScriptNoAddress(t *testing.T) {
	c := cfg.Default()
	c.Net.IPv6Subnet = "2602:fada:6::/64"
	m := &Manager{cfg: c}
	if s, err := m.ipv6ContainerScript(""); err != nil || s != "" {
		t.Errorf(`ipv6ContainerScript("") = %q, %v; want "", nil`, s, err)
	}
}

// The pool container script must configure BOTH guest stacks: Debian gets the
// systemd-networkd files binding the public /128 + fe80::1 route on eth0 and
// DHCPv4 on eth1; RHEL-family images (NetworkManager) get the same layout as a
// static nmcli connection — the networkd files alone would be ignored there,
// leaving the routed NIC without its /128 and the container with no public IPv6.
func TestPoolContainerScript(t *testing.T) {
	c := cfg.Default()
	m := &Manager{cfg: c}
	addr := "2a03:b0c0:2:f0:0:1:dbc2:1006"
	script, err := m.poolContainerScript(addr)
	if err != nil {
		t.Fatal(err)
	}
	// Both stacks: the branch selection and the address/gateway/DHCP markers.
	for _, want := range []string{
		"nmcli",                           // RHEL path present
		"ipv6.method manual",              // RHEL: NM owns the IPv6 stack
		"ipv6.addresses " + addr + "/128", // public /128 bound on eth0
		"ipv6.gateway fe80::1",            // routed-NIC gateway
		"ipv4.method disabled",            // v4 lives on eth1 only
		"systemd-networkd",                // Debian path present
		"Address=" + addr + "/128",        // Debian: static bind
		"Gateway=fe80::1",                 // Debian: default route
		"DHCP=ipv4",                       // eth1 DHCPv4 on both stacks
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	// An empty address is a no-op (V4-only container) — the empty check lives in
	// ipv6ContainerScriptFor, the pool script's only caller.
	if s, _ := m.ipv6ContainerScriptFor("", ""); s != "" {
		t.Errorf("expected empty script for empty address, got %q", s)
	}
}

// An index of 0 is the block the bridge gateway sits in, so it must never reach
// a container. A row carrying it — a V4-only or pool-mode account on a prefix
// host, since 0 is what "no block" looks like there — yields no block and no
// address instead of handing the container the gateway's own address.
func TestIPv6IndexZeroIsNotABlock(t *testing.T) {
	m := ipv6TestManager(t, "2602:fada:6::/64", map[string]int64{"alice": 0})
	block, err := m.IPv6Block("alice")
	if err != nil {
		t.Fatal(err)
	}
	if block != nil {
		t.Errorf("IPv6Block with index 0 = %v, want nil", block)
	}
	addr, err := m.IPv6Addr("alice")
	if err != nil {
		t.Fatal(err)
	}
	if addr != "" {
		t.Errorf("IPv6Addr with index 0 = %q, want no address", addr)
	}

	// ...and block 0 is indeed where the gateway lives, which is why it is
	// refused rather than served.
	n, err := m.cfg.IPv6Network()
	if err != nil {
		t.Fatal(err)
	}
	gw, err := m.bridgeGateway(n)
	if err != nil {
		t.Fatal(err)
	}
	zero, err := m.ipv6BlockIdx(0)
	if err != nil {
		t.Fatal(err)
	}
	if gwIP := net.ParseIP(gw); gwIP == nil || !zero.Contains(gwIP) {
		t.Errorf("block 0 (%v) does not contain the gateway %s — the guard would be guarding nothing", zero, gw)
	}
}

// New accounts never get index 0 either: the picker rejects any index whose
// block holds the gateway, so this holds on an empty database too (where
// nothing else is excluded).
func TestPickIPv6IndexSkipsGatewayBlock(t *testing.T) {
	m := ipv6TestManager(t, "2602:fada:6::/64", nil)
	for i := 0; i < 200; i++ {
		idx, err := m.pickIPv6Index()
		if err != nil {
			t.Fatal(err)
		}
		if idx == 0 {
			t.Fatalf("pickIPv6Index handed out 0, the bridge gateway's block")
		}
	}
}
