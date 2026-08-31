package ndp

import (
	"encoding/binary"
	"net"
	"os"
	"path/filepath"
	"testing"
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
