package ndp

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadRulesAndMatches(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ndppd.conf")
	contents := "proxy eth0 {\n rule 2001:db8:1::/112 {\n static\n }\n}\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	rules, err := loadRules(path)
	if err != nil {
		t.Fatal(err)
	}
	if !matches(net.ParseIP("2001:db8:1::42"), rules) {
		t.Fatal("address inside rule did not match")
	}
	if matches(net.ParseIP("2001:db8:2::42"), rules) {
		t.Fatal("address outside rule matched")
	}
}

// The A-fix: the responder must only ever advertise into the operator's own
// routed prefix. A rule outside `allowed` is dropped even if the rules file (a
// panel-writable path) names it — so a compromised writer cannot turn the root
// raw-socket responder into an NDP spoofer for arbitrary external addresses.
func TestFilterRules(t *testing.T) {
	// A rule inside the allowed prefix is kept.
	inside := []net.IPNet{*mustCIDR(t, "2001:db8:aaaa::1/128")}
	allowed := mustCIDR(t, "2001:db8:aaaa::/48")
	if got := filterRules(inside, allowed); len(got) != 1 {
		t.Fatalf("in-prefix rule dropped: %v", got)
	}
	// A rule outside (or straddling the boundary) is dropped.
	outside := []net.IPNet{
		*mustCIDR(t, "2001:db8:bbbb::1/128"),
		*mustCIDR(t, "2001:db8:aaa1::1/128"),
		*mustCIDR(t, "2002:db8:aaaa::1/128"),
	}
	if got := filterRules(outside, allowed); len(got) != 0 {
		t.Fatalf("out-of-prefix rule kept: %v", got)
	}
	// Mix: only the in-prefix one survives, in order.
	mixed := append(append([]net.IPNet{}, inside...), outside[0])
	if got := filterRules(mixed, allowed); len(got) != 1 || !got[0].Contains(net.ParseIP("2001:db8:aaaa::1")) {
		t.Fatalf("mixed filtering wrong: %v", got)
	}
	// nil allowed = constrained-less test/unconstrained mode: keep everything.
	if got := filterRules(outside, nil); len(got) != len(outside) {
		t.Fatalf("nil allowed should keep all rules, got %v", got)
	}
}

// TestLoadRulesAcceptsPreResponderFile pins the UPGRADE path from a pre-1.6
// (Traefik-era, distro-ndppd) install: that panel rendered each rule with the
// bridge as a nested `iface` argument and three-space indentation —
//
//	rule 2001:db8:1::/112 {
//	   iface incusbr0
//	}
//
// The distro daemon is gone now, but the FILE is not regenerated during an
// upgrade, so the in-tree responder has to read exactly this on the first boot
// after `install.sh`. If the rule indentation or the nested block ever became
// significant, every existing container would lose inbound IPv6 the moment the
// new responder took over.
func TestLoadRulesAcceptsPreResponderFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ndppd.conf")
	legacy := "proxy eth0 {\n" +
		"   rule 2001:db8:1::/112 {\n" +
		"      iface incusbr0\n" +
		"   }\n" +
		"   rule 2001:db8:2::/112 {\n" +
		"      iface incusbr0\n" +
		"   }\n" +
		"}\n"
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	rules, err := loadRules(path)
	if err != nil {
		t.Fatalf("legacy ndppd file rejected: %v", err)
	}
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2", len(rules))
	}
	for _, addr := range []string{"2001:db8:1::1", "2001:db8:1::9999", "2001:db8:2::5"} {
		if !matches(net.ParseIP(addr), rules) {
			t.Errorf("%s did not match any legacy rule", addr)
		}
	}
	if matches(net.ParseIP("2001:db8:3::1"), rules) {
		t.Error("address outside every legacy rule matched")
	}
}

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestSendAdvertisementUsesTargetAsSource(t *testing.T) {
	target := net.ParseIP("2001:db8:1::42")
	peer := net.ParseIP("fe80::2")
	ns := make([]byte, 14+40+24)
	copy(ns[6:12], []byte{0, 1, 2, 3, 4, 5})
	copy(ns[22:38], peer.To16())
	ns[12], ns[13] = 0x86, 0xdd
	ns[20] = 58
	ns[14+40] = icmpv6NS
	copy(ns[14+40+8:14+40+24], target.To16())

	// Build through the same helper used by the raw socket sender; this locks
	// down the wire-format invariants that matter to the provider router.
	mac := [6]byte{0x10, 0x66, 0x6a, 0xf4, 0x1b, 0x4a}
	frame, _, err := buildAdvertisement(mac, ns, target)
	if err != nil {
		t.Fatal(err)
	}
	ipv6 := frame[14:54]
	icmp := frame[54:]

	if got := net.IP(ipv6[8:24]); !got.Equal(target) {
		t.Fatalf("NA source = %s, want %s", got, target)
	}
	if !net.IP(ipv6[24:40]).Equal(peer) {
		t.Fatalf("NA destination is not the NS peer")
	}
	if binary.BigEndian.Uint32(icmp[4:8]) != naFlags {
		t.Fatal("NA flags lost router/solicited/override bits")
	}
}

// A gratuitous advertisement must be a valid NA a router can act on: the
// advertised address as the IPv6 source, the all-nodes group as destination,
// Solicited clear, Override set (so even a REACHABLE-but-wrong cache entry is
// replaced), and the host MAC in the target link-layer option. This is the
// packet that repairs an upstream whose neighbour cache is stale.
func TestBuildUnsolicitedAdvertisement(t *testing.T) {
	target := net.ParseIP("2001:db8:1::1")
	mac := [6]byte{0x10, 0x66, 0x6a, 0xf4, 0x1b, 0x4a}
	frame, dstMAC, err := buildUnsolicitedAdvertisement(mac, target)
	if err != nil {
		t.Fatal(err)
	}
	wantDstMAC := [6]byte{0x33, 0x33, 0x00, 0x00, 0x00, 0x01}
	if dstMAC != wantDstMAC {
		t.Fatalf("destination MAC = %x, want the all-nodes group", dstMAC)
	}
	if !bytes.Equal(frame[0:6], dstMAC[:]) {
		t.Fatalf("ethernet destination = %x", frame[0:6])
	}
	if !bytes.Equal(frame[6:12], mac[:]) {
		t.Fatalf("ethernet source = %x", frame[6:12])
	}

	ipv6 := frame[ethernetHeaderLen : ethernetHeaderLen+ipv6HeaderLen]
	if !net.IP(ipv6[8:24]).Equal(target) {
		t.Fatalf("IPv6 source = %s, want the advertised address", net.IP(ipv6[8:24]))
	}
	if !net.IP(ipv6[24:40]).Equal(net.ParseIP("ff02::1")) {
		t.Fatalf("IPv6 destination = %s, want ff02::1", net.IP(ipv6[24:40]))
	}
	if ipv6[6] != 58 || ipv6[7] != 255 {
		t.Fatalf("next header = %d, hop limit = %d (want 58/255)", ipv6[6], ipv6[7])
	}

	icmp := frame[ethernetHeaderLen+ipv6HeaderLen:]
	if icmp[0] != icmpv6NA {
		t.Fatalf("ICMPv6 type = %d, want NA (%d)", icmp[0], icmpv6NA)
	}
	flags := binary.BigEndian.Uint32(icmp[4:8])
	if flags&0x40000000 != 0 {
		t.Error("Solicited flag must be clear on an unsolicited advertisement")
	}
	if flags&0x20000000 == 0 {
		t.Error("Override flag must be set so a REACHABLE entry is replaced")
	}
	if !net.IP(icmp[8:24]).Equal(target) {
		t.Fatalf("NA target = %s", net.IP(icmp[8:24]))
	}
	if icmp[24] != 2 || icmp[25] != 1 || !bytes.Equal(icmp[26:32], mac[:]) {
		t.Errorf("target link-layer option = %x, want type 2 len 1 mac %x", icmp[24:32], mac)
	}

	// The ICMPv6 checksum must verify over the pseudo-header + message.
	pseudo := make([]byte, 0, 40+len(icmp))
	pseudo = append(pseudo, ipv6[8:24]...)
	pseudo = append(pseudo, ipv6[24:40]...)
	var upper [4]byte
	binary.BigEndian.PutUint32(upper[:], uint32(len(icmp)))
	pseudo = append(pseudo, upper[:]...)
	pseudo = append(pseudo, 0, 0, 0, 58)
	pseudo = append(pseudo, icmp...)
	if checksum(pseudo) != 0 {
		t.Error("ICMPv6 checksum does not verify")
	}
}

// announceTarget advertises a block's primary (network + 1), the address the
// container sources its traffic from, and the address itself for a /128 rule.
func TestAnnounceTargetUsesBlockPrimary(t *testing.T) {
	cases := map[string]string{
		"2001:db8:1::/112":         "2001:db8:1::1",
		"2001:db8:2::/64":          "2001:db8:2::1",
		"2001:db8:0:1:0:2:3:0/112": "2001:db8:0:1:0:2:3:1",
		"2001:db8:1::5/128":        "2001:db8:1::5",
	}
	for cidr, want := range cases {
		_, block, err := net.ParseCIDR(cidr)
		if err != nil {
			t.Fatal(err)
		}
		got := announceTarget(*block)
		if !got.Equal(net.ParseIP(want)) {
			t.Errorf("announceTarget(%s) = %s, want %s", cidr, got, want)
		}
		if !block.Contains(got) {
			t.Errorf("announceTarget(%s) = %s is outside the block", cidr, got)
		}
	}
}

// A block is announced once when it appears — not on every pass — every block
// is refreshed after announceInterval, and a removed block is forgotten so a
// re-add announces it again. A failed send leaves the block unannounced so it
// is retried rather than silently dropped.
func TestAnnounceStateSyncsNewBlocksAndRefreshes(t *testing.T) {
	a := &announceState{last: map[string]time.Time{}, at: time.Now()}
	var sent []string
	send := func(ip net.IP) error {
		sent = append(sent, ip.String())
		return nil
	}
	rules := []net.IPNet{*mustCIDR(t, "2001:db8:1::/112")}

	a.sync(rules, send)
	if len(sent) != 1 || sent[0] != "2001:db8:1::1" {
		t.Fatalf("first sync sent %v, want [2001:db8:1::1]", sent)
	}

	sent = nil
	a.sync(rules, send)
	if len(sent) != 0 {
		t.Fatalf("unchanged pass re-announced: %v", sent)
	}

	rules = append(rules, *mustCIDR(t, "2001:db8:2::/64"))
	sent = nil
	a.sync(rules, send)
	if len(sent) != 1 || sent[0] != "2001:db8:2::1" {
		t.Fatalf("new-block pass sent %v, want [2001:db8:2::1]", sent)
	}

	a.at = time.Now().Add(-announceInterval - time.Second)
	sent = nil
	a.sync(rules, send)
	if len(sent) != 2 {
		t.Fatalf("refresh pass sent %v, want both blocks", sent)
	}

	sent = nil
	a.sync(rules[:1], send)
	if len(sent) != 0 {
		t.Fatalf("removal pass sent %v, want none", sent)
	}
	sent = nil
	a.sync(rules, send)
	if len(sent) != 1 || sent[0] != "2001:db8:2::1" {
		t.Fatalf("re-add pass sent %v, want [2001:db8:2::1]", sent)
	}

	failing := &announceState{last: map[string]time.Time{}, at: time.Now()}
	calls := 0
	fail := func(net.IP) error { calls++; return errors.New("boom") }
	failing.sync(rules[:1], fail)
	failing.sync(rules[:1], fail)
	if calls != 2 {
		t.Fatalf("failed sends = %d, want 2 (block retried while unannounced)", calls)
	}
}
