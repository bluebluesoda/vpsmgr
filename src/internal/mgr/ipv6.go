package mgr

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
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
//   Each container owns a /112 block cut from the prefix for its account: a
//   random 32-bit block index at bits 80-111 plus 16 host bits, stored in
//   users.ipv6_index, so the block is stable across reinstalls but does not
//   spell out the username. The block's primary address (block + ::1) is
//   byte-identical to the address of the old single-/128 scheme for accounts
//   that predate the column, so upgrading never changes an existing
//   container's address.
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
//   changes.

// blockBits is the length of the routed prefix each container owns.
const blockBits = 112

// ipv6BlockIdx builds the /112 block for a stored 32-bit index. The index
// lands at bits 80-111 for every supported prefix <= /80, so the same code
// serves a /64 provider prefix and an /80 slice; the trailing 16 bits are the
// container's own host space. Pure — no Incus, no database.
func (m *Manager) ipv6BlockIdx(idx int64) (*net.IPNet, error) {
	if m.cfg.IPv6ModeEffective() != cfg.IPv6ModePrefix {
		return nil, nil // only prefix mode gives a container a block
	}
	n, err := m.cfg.IPv6Network()
	if err != nil {
		return nil, err
	}
	block := make(net.IP, 16)
	copy(block, n.IP.To16())
	binary.BigEndian.PutUint32(block[10:14], uint32(idx))
	return &net.IPNet{IP: block, Mask: net.CIDRMask(blockBits, 128)}, nil
}

// IPv6Block returns the /112 block a container owns: the index stored with its
// account, placed inside the configured prefix.
//
// The index is random and stored rather than derived from the username. A
// derived one made the suffix of an address a global constant for a given name
// — every host running this panel would give "alice" the same block — so an
// address identified its owner to anyone who knew the scheme. Accounts that
// predate the column were seeded with the value they had been using, which is
// what keeps their addresses stable.
func (m *Manager) IPv6Block(name string) (*net.IPNet, error) {
	if m.cfg.IPv6ModeEffective() != cfg.IPv6ModePrefix {
		return nil, nil // no block — and no reason to touch the database
	}
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return nil, err
	}
	if u.IPv6Index == 0 {
		// No block: a pool-mode or V4-only account stores its address
		// elsewhere (or has none), and 0 is the block the bridge gateway lives
		// in, which is never handed to a container. Serving that block here
		// would put the gateway address on the container.
		return nil, nil
	}
	return m.ipv6BlockIdx(u.IPv6Index)
}

// pickIPv6Index chooses a random 32-bit block index that no account holds and
// whose block does not contain the bridge gateway — a container owning that
// block could bind the gateway address and break routing for everyone. The
// unique index on the column is the backstop; this loop is what keeps a
// collision from reaching it, which is how a name can never be refused for a
// reason as opaque as "your address is taken".
func (m *Manager) pickIPv6Index() (int64, error) {
	if m.cfg.IPv6ModeEffective() != cfg.IPv6ModePrefix {
		return 0, nil // this host hands out no blocks
	}
	users, err := m.db.ListUsers()
	if err != nil {
		return 0, err
	}
	used := make(map[uint32]bool, len(users))
	for _, u := range users {
		used[uint32(u.IPv6Index)] = true
	}
	var gwIP net.IP
	if n, err := m.cfg.IPv6Network(); err == nil {
		if gw, err := m.bridgeGateway(n); err == nil {
			gwIP = net.ParseIP(gw)
		}
	}
	for attempt := 0; attempt < 64; attempt++ {
		n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 32))
		if err != nil {
			return 0, err
		}
		idx := uint32(n.Int64())
		if used[idx] {
			continue
		}
		block, err := m.ipv6BlockIdx(int64(idx))
		if err != nil {
			return 0, err
		}
		if block != nil && gwIP != nil && block.Contains(gwIP) {
			continue
		}
		return int64(idx), nil
	}
	return 0, fmt.Errorf("could not find a free IPv6 block index after 64 tries")
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

// enableProxyNDP turns on kernel proxy_ndp on the external interface: pool
// mode answers upstream neighbor solicitations for container /128s, so the
// upstream router resolves the container address to the host's MAC. Without
// it, inbound ICMP works by luck (the router already has the neighbor) but
// sustained traffic fails, and outbound from the container never gets a return
// path. Goes through the `vps ip6 proxy-ndp-on` root helper (validated
// interface name, pinned sudoers entry) — net.ipv6.conf.all does NOT
// propagate to existing interfaces.
func (m *Manager) enableProxyNDP() error {
	ext := m.cfg.Net.ExtIF
	if ext == "" {
		return fmt.Errorf("no external interface for proxy_ndp")
	}
	_, err := su.IP6("proxy-ndp-on", ext, ext)
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

// ndppdConf renders /etc/ndppd.conf: one `rule <cidr>` per block a container
// owns — its deterministic /112 and, when it has one, its whole /64 from
// net.ipv6_extra_prefix — under a `proxy <ext_if>` section, so the in-tree NDP
// responder knows which prefixes to answer for on the external link. `fresh`
// adds blocks whose account row does not exist yet (Add wires the container
// before writing its row, so it passes what it just picked, comma-separated)
// and `drop` leaves one user out by name (its row still exists while Del
// unwires the container).
// Empty when IPv6 is disabled or no container has a block. The format is kept
// ndppd-compatible (each rule line is a bare `rule <cidr> {`), even though the
// daemon is no longer used in prefix mode.
func (m *Manager) ndppdConf(fresh, drop string) (string, error) {
	if !m.cfg.IPv6Enabled() {
		return "", nil
	}
	ext := m.cfg.Net.ExtIF
	if ext == "" {
		return "", fmt.Errorf("no external interface for ndppd")
	}
	users, err := m.db.ListUsers()
	if err != nil {
		return "", err
	}
	blocks := make([]string, 0, len(users)+1)
	for _, u := range users {
		if u.Name == drop {
			continue
		}
		block, err := m.IPv6Block(u.Name)
		if err != nil {
			return "", err
		}
		if block != nil {
			blocks = append(blocks, block.String())
		}
		// The whole /64 is routed to the container the same way the /112 is, so
		// an upstream that resolves the prefix by NDP has to be answered for it
		// too.
		if u.IPv6ExtraBlock != "" {
			blocks = append(blocks, u.IPv6ExtraBlock)
		}
	}
	if fresh != "" {
		blocks = append(blocks, strings.Split(fresh, ",")...)
	}
	if len(blocks) == 0 {
		return "", nil
	}
	sort.Strings(blocks)
	var b strings.Builder
	b.WriteString(cfg.GeneratedBanner)
	fmt.Fprintf(&b, "proxy %s {\n", ext)
	for _, block := range blocks {
		fmt.Fprintf(&b, "   rule %s {\n      iface %s\n   }\n", block, m.cfg.Incus.Bridge)
	}
	b.WriteString("}\n")
	return b.String(), nil
}

// writeNDPPD renders the config for the current container set (plus/minus one
// container) and atomically rewrites the file. The in-tree NDP responder
// re-reads the file while it runs (vps add/del never restarts it), so the
// rewrite must be atomic: a unique temp file in the (vps-writable) config dir
// is renamed over the target, so the responder never reads a half-written rule
// set and the unprivileged panel never depends on overwriting a possibly
// root-owned old file (rename checks the parent dir, not the target). When no
// container has IPv6 routing the file is removed, so a stale rule can never
// misroute.
func (m *Manager) writeNDPPD(fresh, drop string) error {
	conf, err := m.ndppdConf(fresh, drop)
	if err != nil {
		return err
	}
	if conf == "" {
		_ = os.Remove(ndppdConfPath)
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Dir(ndppdConfPath), ".ndppd.conf.*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(conf); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, ndppdConfPath); err != nil {
		return err
	}
	removeTemp = false
	return nil
}

// WireIPv6 registers a container's blocks with the NDP proxy so its addresses
// are reachable from the internet: its /112, and the whole /64 it may own from
// net.ipv6_extra_prefix. Both are passed explicitly by Add, which wires the
// container before its account row exists; with nil they are read from the
// account (the stored /112 index and the stored /64). The Incus device routes
// the /112, and WireExtraBlock routes the /64.
func (m *Manager) WireIPv6(name string, block, extra *net.IPNet) error {
	if !m.cfg.IPv6Enabled() {
		return nil
	}
	if block == nil {
		b, err := m.IPv6Block(name)
		if err != nil {
			return err
		}
		block = b
	}
	var fresh []string
	if block != nil {
		fresh = append(fresh, block.String())
	}
	if extra != nil {
		fresh = append(fresh, extra.String())
	}
	if len(fresh) == 0 {
		return nil
	}
	return m.writeNDPPD(strings.Join(fresh, ","), "")
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
// pass-through survives reboots. Idempotent. In pool mode the host plumbing
// is just "pool addresses are not bound on the external interface + proxy_ndp
// / forwarding on" (the routed NICs program their own per-address routes).
func (m *Manager) RewireAllIPv6() error {
	if !m.cfg.IPv6Enabled() {
		return nil
	}
	if m.cfg.IPv6ModeEffective() == cfg.IPv6ModePool {
		return m.RewireAllIPv6Pool()
	}
	if err := m.SetupIPv6Bridge(); err != nil {
		return err
	}
	m.cleanLegacyKernelProxy()
	if err := m.writeNDPPD("", ""); err != nil {
		return err
	}
	// The whole /64 blocks are installed by the panel, not by Incus, so nothing
	// else would bring them back after a reboot.
	return m.EnsureExtraBlockRoutes()
}
