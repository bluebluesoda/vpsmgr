package mgr

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/su"
)

// IPv6 pass-through support (verified empirically):
//
//   incusbr0 is configured with the GLOBAL prefix — /64 or shorter (e.g. /56),
//   or the /80 slice a provider hands the host — with ipv6.routing + stateful
//   DHCPv6. For a shorter prefix (/48 /56 /60) the bridge carries the FIRST
//   /64 slice of it, because Incus's dnsmasq rejects non-/64 networks and every
//   deterministic container address falls in that /64 anyway.
//
//   Each container owns a DETERMINISTIC /112 block derived from its username
//   (sha256 → 32-bit block index at bits 80-111 + 16 host bits), so the block
//   is stable across reinstalls and never stored or queried. The block's
//   primary address (block + ::1) is byte-identical to the address of the old
//   single-/128 scheme, so upgrading never changes an existing container's
//   address.
//
//   Per container:
//     - the eth0 device sets ipv6.address=<block>::1 (primary) and
//       ipv6.routes=<block>::/112, so Incus routes the whole block to the
//       container and any address it binds is delivered to it.
//     - ndppd proxies Neighbor Discovery on the EXTERNAL interface for every
//       /112: an upstream neighbor solicitation for an address in a block is
//       relayed to the bridge, the container answers, ndppd relays the NA
//       back. Kernel proxy_ndp only answers single addresses (verified: it
//       ignores prefix- or route-covered queries), which is why ndppd is used.
//
//   vpsmgr renders /etc/ndppd.conf (one rule per container) and restarts the
//   daemon on add/del/reapply; RewireAllIPv6 rebuilds it from the DB at boot
//   and on `vps install`, so rules survive reboots. No NAT, no nftables
//   changes, no DB schema changes.

// ipv6Suffix returns the low 48 host bits of a container's primary IPv6 (the
// 32-bit username hash followed by a fixed 0001 last block). Kept for
// tests/diagnostics; IPv6Addr writes the same bits directly into the address.
func ipv6Suffix(name string) string {
	h := sha256.Sum256([]byte(name))
	v := binary.BigEndian.Uint32(h[:4])
	return fmt.Sprintf("%x:%04x:1", v>>16, v&0xffff)
}

// blockBits is the length of the routed prefix each container owns.
const blockBits = 112

// IPv6Block computes the deterministic /112 block a container owns, derived
// from the configured prefix + username. Never queries Incus and never stores
// it. The block index (32-bit username hash) lands at bits 80-111 for every
// supported prefix <= /80; the trailing 16 bits are the container's host
// space.
func (m *Manager) IPv6Block(name string) (*net.IPNet, error) {
	if !m.cfg.IPv6Enabled() {
		return nil, nil
	}
	n, err := m.cfg.IPv6Network()
	if err != nil {
		return nil, err
	}
	block := make(net.IP, 16)
	copy(block, n.IP.To16())
	h := sha256.Sum256([]byte(name))
	copy(block[10:14], h[:4])
	return &net.IPNet{IP: block, Mask: net.CIDRMask(blockBits, 128)}, nil
}

// IPv6Addr returns the container's primary global address — its /112 block
// plus ::1 — used as the eth0 DHCPv6 reservation. Byte-identical to the
// pre-/112 scheme, so existing container addresses never change.
func (m *Manager) IPv6Addr(name string) (string, error) {
	b, err := m.IPv6Block(name)
	if err != nil || b == nil {
		return "", err
	}
	return addHostOffset(b.IP, 1).String(), nil
}

// SetupIPv6Bridge configures incusbr0 for IPv6 pass-through. Idempotent.
func (m *Manager) SetupIPv6Bridge() error {
	if !m.cfg.IPv6Enabled() {
		return nil
	}
	n, err := m.cfg.IPv6Network()
	if err != nil {
		return err
	}
	// The bridge gateway is a free address inside the prefix — normally net+1,
	// but skipped when the host itself already uses it on the external
	// interface (common with a /80 slice where the host holds ::1).
	//
	// Bridge prefix length: Incus's dnsmasq only serves /64 networks (a shorter
	// prefix like /48 /56 /60 makes it error "only /64 allowed"). Since every
	// deterministic container address lives in the FIRST /64 of the configured
	// prefix (bits [prefixlen:79] are zero-filled), we clamp the bridge to /64
	// for those — containers still fall inside it. /64 and /80 use their own
	// length.
	ones, _ := n.Mask.Size()
	bridgeOnes := bridgePrefixLen(ones)
	gw, err := m.bridgeGateway(n)
	if err != nil {
		return err
	}
	bridge := m.cfg.Incus.Bridge
	for _, kv := range []string{
		"ipv6.address=" + gw + "/" + strconv.Itoa(bridgeOnes),
		"ipv6.nat=false",
		"ipv6.routing=true",
		"ipv6.dhcp.stateful=true",
	} {
		if err := m.lx.NetworkSet(bridge, kv); err != nil {
			return err
		}
	}
	// Route the bridge's own prefix through the bridge so Incus can program
	// the per-container /112 routes (ipv6.routes). On LXD the bridge address
	// auto-created this route; Incus 7.0 does not when the external interface
	// already holds an equal-prefix route, and without a dev incusbr0 route the
	// container's ipv6.routes cannot be installed ("no route to host"). Adding
	// the route is idempotent (EEXIST is fine). Needs CAP_NET_ADMIN → sudoers
	// whitelist.
	bridgeNet := &net.IPNet{IP: net.ParseIP(gw).Mask(net.CIDRMask(bridgeOnes, 128)), Mask: net.CIDRMask(bridgeOnes, 128)}
	if err := m.ipRouteAdd(bridgeNet.String(), bridge); err != nil {
		return fmt.Errorf("add bridge route %s dev %s: %w", bridgeNet.String(), bridge, err)
	}
	// Give the bridge a fixed link-local address (fe80::1) so containers can
	// statically point their default route at it — no dependency on learning
	// the gateway from router advertisements.
	if _, err := su.IP6("addr-add", "fe80::1/64", bridge); err != nil && !isExistsErr(err) {
		return fmt.Errorf("add bridge link-local address: %w", err)
	}
	if err := m.enableForwarding(); err != nil {
		return fmt.Errorf("enable ipv6 forwarding: %w", err)
	}
	return nil
}

// ipRouteAdd adds an IPv6 route, tolerating "already exists" (the command is
// idempotent across installs/reapplies) but failing on any real error.
func (m *Manager) ipRouteAdd(route, dev string) error {
	_, err := su.IP6("route-add", route, dev)
	if err != nil && !isExistsErr(err) {
		return err
	}
	return nil
}

// isExistsErr reports whether a su.Run/su.IP6 error is the kernel's
// "already exists / already assigned" (route/address/neighbor), which is the
// idempotent no-op case for these setup commands — any other failure is a
// real error.
func isExistsErr(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "file exists") ||
		strings.Contains(msg, "already exist") ||
		strings.Contains(msg, "already assigned") ||
		strings.Contains(msg, "exists") ||
		strings.Contains(msg, "einval") && strings.Contains(msg, "address")
}

// bridgeGateway picks the first usable address inside the prefix (net+1,
// net+2, ...) that is not already taken by anything the host can see:
//   - addresses assigned to the host's external interface (e.g. a /80 slice
//     where the host itself holds ::1)
//   - the upstream default gateway(s) — a global gateway inside the prefix
//     (very common with a /64, where the ISP's router is at ::1) must never be
//     claimed by the bridge, or the host would answer for it and break its own
//     outbound routing
//   - any address present in the NDP neighbor table on the external interface
//     (catches the router and any other device already on the link)
//
// A container's hash-derived address is 2^-32 unlikely to collide with any of
// these (and its 0001 last block can never be the all-zero anycast), and Incus
// only uses this address as the dnsmasq/SLAAC anchor.
func (m *Manager) bridgeGateway(n *net.IPNet) (string, error) {
	inUse := map[string]bool{}
	ext := m.cfg.Net.ExtIF

	// 1. Addresses the host itself holds on the external interface.
	if ext != "" {
		out, err := exec.Command("ip", "-6", "-o", "addr", "show", "dev", ext, "scope", "global").CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("list ipv6 addrs on %s: %s", ext, strings.TrimSpace(string(out)))
		}
		for _, f := range strings.Fields(string(out)) {
			addr := strings.SplitN(f, "/", 2)[0]
			if ip := net.ParseIP(addr); ip != nil {
				inUse[ip.String()] = true
			}
		}
	}

	// 2. Default gateway(s) — `via` addresses in `ip -6 route show default`.
	if out, err := exec.Command("ip", "-6", "route", "show", "default").CombinedOutput(); err == nil {
		for _, f := range strings.Fields(string(out)) {
			if ip := net.ParseIP(f); ip != nil {
				inUse[ip.String()] = true
			}
		}
	}

	// 3. Already-resolved neighbors on the upstream link (router, other hosts).
	if ext != "" {
		if out, err := exec.Command("ip", "-6", "neigh", "show", "dev", ext).CombinedOutput(); err == nil {
			for _, f := range strings.Fields(string(out)) {
				if ip := net.ParseIP(f); ip != nil {
					inUse[ip.String()] = true
				}
			}
		}
	}

	base := n.IP.To16()
	for k := uint64(1); k < 1<<16; k++ {
		ip := addHostOffset(base, k)
		if !inUse[ip.String()] {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("no free gateway address in %s", n.String())
}

// addHostOffset returns the network address + k, incrementing the low 64 host
// bits big-endian with carry. k only ever touches host bits for any prefix
// <= /80 (48+ host bits), so the result stays inside the subnet.
func addHostOffset(netAddr net.IP, k uint64) net.IP {
	ip := make(net.IP, 16)
	copy(ip, netAddr)
	for i := 15; i >= 8 && k > 0; i-- {
		v := uint64(ip[i]) + (k & 0xff)
		ip[i] = byte(v & 0xff)
		k >>= 8
		if v > 0xff {
			k++
		}
	}
	return ip
}

// bridgePrefixLen clamps the configured prefix length for the incusbr0 bridge.
// Incus's dnsmasq only serves /64 networks (a /48 /56 /60 makes it error "only
// /64 allowed"), and every deterministic container address falls inside the
// FIRST /64 of the configured prefix — so shorter prefixes ride on that /64;
// /64 and /80 keep their own length.
func bridgePrefixLen(ones int) int {
	if ones < 64 {
		return 64
	}
	return ones
}

// enableForwarding turns on IPv6 forwarding (required for pass-through).
// sysctl write needs root → sudoers whitelist.
func (m *Manager) enableForwarding() error {
	_, err := su.Run("/sbin/sysctl", "-w", "net.ipv6.conf.all.forwarding=1")
	return err
}

// ndppdConfPath is where vpsmgr renders the ndppd rules. It lives inside
// /etc/vpsmgr (the panel's own writable dir) instead of /etc/ndppd.conf in the
// root-owned area of the filesystem — the panel daemon generates this file, so
// giving it a root-zone path just to chown it back would widen the "unprivileged
// user writes root-consumed files" surface for no gain.
//
// ndppd itself reads /etc/ndppd.conf by default (its init script starts it with
// bare `-d -p $PIDFILE`, no -c flag), so a root-owned SYMLINK /etc/ndppd.conf →
// /etc/vpsmgr/ndppd.conf is maintained alongside the real file. The link is
// created/removed through the sudoers whitelist with pinned commands only.
const ndppdConfPath = "/etc/vpsmgr/ndppd.conf"
const ndppdConfLink = "/etc/ndppd.conf"

// linkNDPPDConf points the daemon's default config path at vpsmgr's rendered
// file. Pinned sudo command (no wildcards), idempotent.
func (m *Manager) linkNDPPDConf() error {
	_, err := su.Run("/bin/ln", "-sf", ndppdConfPath, ndppdConfLink)
	return err
}

// unlinkNDPPDConf removes the root-owned symlink (pinned sudo rm). A missing
// file is not an error.
func (m *Manager) unlinkNDPPDConf() error {
	_, err := su.Run("/bin/rm", "-f", ndppdConfLink)
	return err
}

// ndppdConf renders /etc/ndppd.conf: one `rule <block>::/112` per container
// under a `proxy <ext_if>` section, so upstream neighbor solicitations for any
// address in a container's block are relayed to the Incus bridge (the container
// answers for the addresses it binds). `add` / `drop` let a single user be
// added or removed without racing the DB transaction in Add/Del. Empty when
// IPv6 is disabled or no container has a block.
func (m *Manager) ndppdConf(add, drop string) (string, error) {
	if !m.cfg.IPv6Enabled() {
		return "", nil
	}
	ext := m.cfg.Net.ExtIF
	if ext == "" {
		return "", fmt.Errorf("no external interface for ndppd")
	}
	names := map[string]bool{}
	users, err := m.db.ListUsers()
	if err != nil {
		return "", err
	}
	for _, u := range users {
		names[u.Name] = true
	}
	if drop != "" {
		delete(names, drop)
	}
	if add != "" {
		names[add] = true
	}
	if len(names) == 0 {
		return "", nil
	}
	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)
	var b strings.Builder
	b.WriteString(cfg.GeneratedBanner)
	fmt.Fprintf(&b, "proxy %s {\n", ext)
	for _, n := range sorted {
		block, err := m.IPv6Block(n)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "   rule %s {\n      iface %s\n   }\n", block.String(), m.cfg.Incus.Bridge)
	}
	b.WriteString("}\n")
	return b.String(), nil
}

// restartNDPPD (re)starts the ndppd daemon after a config write. ndppd 0.2.x
// has no live reload (SIGHUP terminates it), so a restart is required and its
// liveness is verified afterwards. The distro init script can wedge in a stale
// "active (exited)" state, so a failed start falls back to launching the
// daemon directly. All calls go through the sudoers whitelist.
func (m *Manager) restartNDPPD() error {
	if _, err := exec.LookPath("ndppd"); err != nil {
		return fmt.Errorf("ndppd is not installed (install.sh installs it when IPv6 is enabled)")
	}
	_, _ = su.Run("/usr/sbin/service", "ndppd", "restart")
	if m.ndppdAlive() {
		return nil
	}
	_, _ = su.Run("/usr/sbin/service", "ndppd", "start")
	if m.ndppdAlive() {
		return nil
	}
	// Last resort: start the daemon directly, bypassing the init script.
	_, _ = su.Run("/usr/bin/pkill", "-x", "ndppd")
	_ = os.Remove("/var/run/ndppd.pid")
	if _, err := su.Run("/usr/sbin/ndppd", "-d", "-p", "/var/run/ndppd.pid"); err != nil {
		return err
	}
	return nil
}

// ndppdAlive reports whether an ndppd process is running.
func (m *Manager) ndppdAlive() bool {
	out, err := exec.Command("pgrep", "-x", "ndppd").Output()
	return err == nil && len(out) > 0
}

// writeNDPPD renders the config for the current container set (plus/minus one
// container) and restarts the daemon. When no container has IPv6 routing the
// daemon is stopped, so a stale config can never misroute.
func (m *Manager) writeNDPPD(add, drop string) error {
	conf, err := m.ndppdConf(add, drop)
	if err != nil {
		return err
	}
	if conf == "" {
		_, _ = su.Run("/usr/sbin/service", "ndppd", "stop")
		_ = os.Remove(ndppdConfPath)
		_ = m.unlinkNDPPDConf()
		return nil
	}
	if err := os.WriteFile(ndppdConfPath, []byte(conf), 0o644); err != nil {
		return err
	}
	// Point the daemon's default config path at the rendered file BEFORE the
	// restart, or ndppd exits with "Failed to load configuration file
	// '/etc/ndppd.conf'" and every WireIPv6/UnwireIPv6 fails.
	if err := m.linkNDPPDConf(); err != nil {
		return err
	}
	return m.restartNDPPD()
}

// WireIPv6 registers a container's /112 with the NDP proxy so its addresses
// are reachable from the internet. The block is computed from the username (no
// waiting); the Incus device already routes the /112 to the container.
func (m *Manager) WireIPv6(name string) error {
	if !m.cfg.IPv6Enabled() {
		return nil
	}
	return m.writeNDPPD(name, "")
}

// UnwireIPv6 removes a container's /112 from the NDP proxy. Returns the error
// so a failed proxy reconfiguration is not silently swallowed in Del/cleanup —
// a leftover ndppd rule would keep answering for a deleted container's block.
func (m *Manager) UnwireIPv6(name string) error {
	if !m.cfg.IPv6Enabled() {
		return nil
	}
	return m.writeNDPPD("", name)
}

// cleanLegacyKernelProxy removes the per-address kernel proxy_ndp entries and
// /128 routes the old (pre-/112) scheme installed on the external interface
// and the bridge. Idempotent, best-effort: the /112 routes programmed by Incus
// supersede both. Only touches addresses inside the configured prefix.
func (m *Manager) cleanLegacyKernelProxy() {
	if !m.cfg.IPv6Enabled() {
		return
	}
	ext := m.cfg.Net.ExtIF
	bridge := m.cfg.Incus.Bridge
	users, err := m.db.ListUsers()
	if err != nil {
		return
	}
	for _, u := range users {
		addr, err := m.IPv6Addr(u.Name)
		if err != nil || addr == "" {
			continue
		}
		if ext != "" {
			_, _ = su.IP6("neigh-del-proxy", addr, ext)
		}
		_, _ = su.IP6("route-del", addr, bridge)
	}
}

// RewireAllIPv6 rebuilds the whole IPv6 pass-through: bridge config, the
// ndppd rules for every container, and a sweep of the old kernel per-address
// plumbing. Called at boot (after Incus is up) and by `vps install` so that
// pass-through survives reboots. Idempotent.
func (m *Manager) RewireAllIPv6() error {
	if !m.cfg.IPv6Enabled() {
		return nil
	}
	if err := m.SetupIPv6Bridge(); err != nil {
		return err
	}
	m.cleanLegacyKernelProxy()
	return m.writeNDPPD("", "")
}
