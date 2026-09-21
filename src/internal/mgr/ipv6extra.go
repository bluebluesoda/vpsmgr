package mgr

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"sort"
	"strings"

	"vpsmgr/internal/cfg"
)

// Whole-/64 blocks from an extra prefix (net.ipv6_extra_prefix).
//
// A prefix shorter than /64 can be sliced into whole /64s, one per container.
// That is the only block size that makes the feature worth having: SLAAC needs
// exactly 64 bits, so a container owning a /64 can run its own router, hand
// addresses to nested containers/VMs and generally be a real IPv6 tenant —
// which a /112 (the normal per-container block, cut from the base /64) cannot.
//
// The feature is OFF unless the operator sets net.ipv6_extra_prefix: nothing is
// allocated, no UI appears, and every upgraded install stays exactly as it was.
//
// How a block is delivered:
//
//   - The container binds <block>::1/64 on its eth0 (see
//     ipv6ContainerScriptFor), so the whole prefix is on-link inside and any
//     address in it is usable. Sub-prefixes the customer carves out internally
//     are more specific than that /64 and win over it.
//   - The host routes <block>/64 to the container by declaring it on the
//     container's NIC: eth0 carries `ipv6.routes = <block>/64,<account's /112>`
//     (that order is load-bearing) and keeps its `ipv6.address` — see
//     applyExtraRoutes for why each part is required. Declaring it there is what
//     makes the bridge's own source filter accept packets the container sources
//     from the block, and it makes Incus reinstall the route on every container
//     start, so the block survives reboots with no panel-side route plumbing.
//   - ndppd gets a rule per block, like the /112s, so an upstream that does NDP
//     for the prefix is answered.
//
// A block is stored as the CIDR actually handed out (users.ipv6_extra_block),
// not re-derived from the config, so changing the prefix later cannot invalidate
// an existing assignment. It is released only by deleting the account — no
// quota edit can take it back.

// extraCountCap caps the reported size of an absurdly short prefix (a /24, say)
// so the panel never shows a meaningless number. The allocator itself never
// depends on it: it scans at most used+reserved+1 indices.
const extraCountCap = 1 << 32

// ExtraPrefixNetwork returns the configured extra prefix, or nil when the
// feature is off. The registry validates the value on write, so an error here
// means a hand-edited config.
func (m *Manager) ExtraPrefixNetwork() (*net.IPNet, error) {
	return m.cfg.IPv6ExtraPrefixNetwork()
}

// ExtraEnabled reports whether a container can be given a whole /64 right now:
// prefix mode, an extra prefix configured, and at least one free block.
func (m *Manager) ExtraEnabled() bool {
	if m.cfg.IPv6ModeEffective() != cfg.IPv6ModePrefix {
		return false
	}
	_, _, _, free, err := m.ExtraCapacity()
	return err == nil && free > 0
}

// ExtraCapacity reports the whole-/64 pool: how many blocks the configured
// prefix holds, how many of those are reserved for this host, how many are
// assigned to containers, and how many are free. All zeros when the feature is
// off or the prefix is a /64 or longer (which fits no whole /64).
func (m *Manager) ExtraCapacity() (total, reserved, used, free int, err error) {
	p, err := m.cfg.IPv6ExtraPrefixNetwork()
	if err != nil || p == nil {
		return 0, 0, 0, 0, err
	}
	// Only prefix mode hands out blocks; in pool mode the config value is inert.
	if m.cfg.IPv6ModeEffective() != cfg.IPv6ModePrefix {
		return 0, 0, 0, 0, nil
	}
	total = int(extraBlockCount(p))
	reserved = len(m.reservedExtraBlocks(p))
	usedSet, err := m.db.UsedIPv6ExtraBlocks()
	if err != nil {
		return 0, 0, 0, 0, err
	}
	// Count only the assignments that still belong to this prefix: the operator
	// may have changed it, and an old block outside it must not make the pool
	// look fuller — or the free count negative.
	for b := range usedSet {
		if _, bn, err := net.ParseCIDR(b); err == nil && p.Contains(bn.IP) {
			used++
		}
	}
	free = total - reserved - used
	if free < 0 {
		free = 0
	}
	return total, reserved, used, free, nil
}

// extraBlockCount returns how many /64s fit in p, or 0 when p is a /64 or
// longer (a /65 holds no whole /64).
func extraBlockCount(p *net.IPNet) uint64 {
	ones, _ := p.Mask.Size()
	if ones >= 64 || ones < 0 {
		return 0
	}
	shift := uint(64 - ones)
	if shift >= 31 {
		// A prefix shorter than /33 — the pool is effectively unlimited. Cap the
		// number the caller computes with and reports.
		return extraCountCap
	}
	return uint64(1) << shift
}

// extraBlockAt returns the idx-th /64 inside p, idx 0 being the lowest. The
// index occupies the bits between the prefix length and the /64 boundary, which
// is exactly what makes the blocks line up with the /64s of the real prefix.
func extraBlockAt(p *net.IPNet, idx uint64) string {
	b := make(net.IP, 16)
	copy(b, p.IP.To16())
	// p.IP is a network address, so the bits below the prefix are already zero
	// and the index can simply be OR-ed into the tail of the /64's first half.
	for i := 7; i >= 0 && idx > 0; i-- {
		b[i] |= byte(idx & 0xff)
		idx >>= 8
	}
	return (&net.IPNet{IP: b, Mask: net.CIDRMask(64, 128)}).String()
}

// reservedExtraBlocks returns the /64s inside extra that must never be handed to
// a container:
//
//   - the /64 the host itself uses for pass-through — the one holding
//     net.ipv6_subnet, which carries the bridge address, the gateway, every
//     container's /112 and, on a provider that delegates a short prefix
//     alongside it, the host's own SLAAC address. Routing it to a container
//     would both steal the host's own addresses and collide with the kernel's
//     existing on-link route for it, breaking the host's upstream connectivity.
//   - every /64 the host currently holds a global address in, which catches the
//     case the first rule misses: net.ipv6_subnet set to one /64 of the extra
//     prefix while the host's own address lives in a different /64 of it.
//
// Both only ever shrink the pool. Blocks outside the extra prefix are not
// returned (they cannot be allocated anyway).
func (m *Manager) reservedExtraBlocks(extra *net.IPNet) map[string]bool {
	out := map[string]bool{}
	add := func(ip net.IP) {
		if ip == nil || ip.To4() != nil || ip.To16() == nil {
			return
		}
		mask := net.CIDRMask(64, 128)
		out[(&net.IPNet{IP: ip.To16().Mask(mask), Mask: mask}).String()] = true
	}
	if n, err := m.cfg.IPv6Network(); err == nil && n != nil {
		add(n.IP)
	}
	// `ip -6 -o addr show scope global` lines look like
	//   2: ens18    inet6 2001:db8::1/64 scope global ...
	// so the fourth field is the address with its prefix length.
	if raw, err := exec.Command("ip", "-6", "-o", "addr", "show", "scope", "global").CombinedOutput(); err == nil {
		for _, f := range strings.Fields(string(raw)) {
			if ip := net.ParseIP(strings.SplitN(f, "/", 2)[0]); ip != nil {
				add(ip)
			}
		}
	}
	for b := range out {
		if _, bn, err := net.ParseCIDR(b); err != nil || !extra.Contains(bn.IP) {
			delete(out, b)
		}
	}
	return out
}

// pickExtraBlock returns the lowest free whole /64 in the extra prefix, or ""
// when every block is taken or reserved (the caller then creates the container
// without one — allocation is best-effort, never an error).
//
// The scan is bounded by used+reserved+1: by pigeonhole a free block must appear
// within that many consecutive indices, so the cost is O(containers) even for a
// /48, and the pick is deterministic — the admin page can show exactly which
// blocks are in use.
func (m *Manager) pickExtraBlock() (string, error) {
	if m.cfg.IPv6ModeEffective() != cfg.IPv6ModePrefix {
		return "", nil // only prefix mode hands out blocks
	}
	p, err := m.cfg.IPv6ExtraPrefixNetwork()
	if err != nil || p == nil {
		return "", err
	}
	total := extraBlockCount(p)
	if total == 0 {
		return "", nil
	}
	used, err := m.db.UsedIPv6ExtraBlocks()
	if err != nil {
		return "", err
	}
	reserved := m.reservedExtraBlocks(p)
	limit := uint64(len(used)+len(reserved)) + 1
	if limit > total {
		limit = total
	}
	for idx := uint64(0); idx < limit; idx++ {
		b := extraBlockAt(p, idx)
		if used[b] || reserved[b] {
			continue
		}
		return b, nil
	}
	return "", nil
}

// ExtraEntry is one assigned whole /64: the block and the account holding it.
type ExtraEntry struct {
	Block string
	User  string
}

// ExtraAssignments lists every assigned block with its owner, for the admin
// page. Sorted by block, so consecutive allocations read in order.
func (m *Manager) ExtraAssignments() []ExtraEntry {
	users, err := m.db.ListUsers()
	if err != nil {
		return nil
	}
	out := make([]ExtraEntry, 0, len(users))
	for _, u := range users {
		if u.IPv6ExtraBlock != "" {
			out = append(out, ExtraEntry{Block: u.IPv6ExtraBlock, User: u.Name})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Block < out[j].Block })
	return out
}

// blockRoutes renders an eth0 ipv6.routes value: the whole /64 first, then the
// account's /112. Incus takes a comma-separated list, and it installs it in the
// order given — which matters, see applyExtraRoutes.
func blockRoutes(block112, extra string) string {
	switch {
	case extra == "":
		return block112
	case block112 == "":
		return extra
	default:
		return extra + "," + block112
	}
}

// applyExtraRoutes rewrites a container's eth0 IPv6 wiring so its whole /64 is
// routed to it: ipv6.routes carries the block AND the account's /112, and
// ipv6.address stays declared. Every part of that shape is load-bearing, and all
// of it was found the hard way:
//
//   - Declaring the /64 is what makes the bridge's source filter
//     (security.ipv6_filtering) accept packets the container sources from it.
//   - ipv6.address must stay declared. Incus programs a declared route as `via
//     <ipv6.address>`, and that is what keeps the /112 usable: the guest makes
//     the whole /112 local (see ipv6ContainerScriptFor), so any address in it
//     answers — but only if the host delivers it to the container's MAC. Drop
//     the address and both routes become direct `dev` routes, so the host has to
//     resolve every single address by neighbour discovery on the bridge, where
//     the guest only answers for the ones it has bound: ::1 works and the rest
//     of the /112 goes silent.
//   - The /64 must come FIRST. The kernel refuses a via-address route whose
//     gateway is itself reached by a via-address route, and the /112 route is
//     exactly that ("No route to host"), so the /64 only installs while it is
//     declared before the /112. Installed, it carries the whole block to the
//     container's MAC — which is what lets the customer route sub-prefixes out
//     of it instead of running a responder for every address in it.
//
// A changed device restarts the container (Incus device patches stop it first),
// so the caller can configure the guest right after. Idempotent: an unchanged
// device is not touched.
func (m *Manager) applyExtraRoutes(name, extra string) error {
	block, err := m.IPv6Block(name)
	if err != nil {
		return err
	}
	if block == nil {
		return fmt.Errorf("%s has no /112 block to route", name)
	}
	primary, err := m.IPv6Addr(name)
	if err != nil {
		return err
	}
	if primary == "" {
		return fmt.Errorf("%s has no IPv6 address to route from", name)
	}
	_, err = m.lx.EnsureEth0Options(name, map[string]string{
		"ipv6.address": primary,
		"ipv6.routes":  blockRoutes(block.String(), extra),
	})
	return err
}

// EnsureExtraBlockRoutes reapplies the eth0 wiring of every account that owns a
// whole /64. Containers that were created (or upgraded) before their block was
// declared would otherwise never get it back: the kernel-side state is part of
// the instance config, but the declaration is a change made after the container
// existed. Called by EnsureBlockRoutes (`vps install`) and by
// RewireAllIPv6 (the boot unit and `vps ipv6-reapply`), so it also heals a
// container that was recreated out of band.
func (m *Manager) EnsureExtraBlockRoutes() error {
	if m.cfg.IPv6ModeEffective() != cfg.IPv6ModePrefix {
		return nil
	}
	users, err := m.db.ListUsers()
	if err != nil {
		return err
	}
	var firstErr error
	for _, u := range users {
		if u.IPv6ExtraBlock == "" {
			continue
		}
		if err := m.applyExtraRoutes(u.Name, u.IPv6ExtraBlock); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// AssignExtraBlock gives an EXISTING container a whole /64 — the admin's
// "quota → assign" path. Idempotent: a container that already has one keeps it
// and no error is raised, so re-submitting the dialog is harmless.
//
// It redeclares the container's NIC (which restarts it: Incus device patches
// do) and then binds the block inside the guest. A guest that refuses the new
// configuration is reported as a warning — the block stays assigned and routed,
// mirroring how a reinstall treats a broken guest.
func (m *Manager) AssignExtraBlock(name string) (string, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	if m.cfg.IPv6ModeEffective() != cfg.IPv6ModePrefix {
		return "", errors.New("this host does not hand out whole /64 blocks (IPv6 prefix mode only)")
	}
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return "", err
	}
	if u.IPv6ExtraBlock != "" {
		return u.IPv6ExtraBlock, nil // already assigned
	}
	block, err := m.pickExtraBlock()
	if err != nil {
		return "", err
	}
	if block == "" {
		return "", errors.New("no free /64 left in the configured extra prefix")
	}
	if err := m.db.UpdateUserIPv6ExtraBlock(u.ID, block); err != nil {
		return "", err
	}
	u.IPv6ExtraBlock = block
	// Re-declare the NIC so the /64 is routed to the container and allowed by
	// the bridge's source filter. This restarts the container.
	if err := m.applyExtraRoutes(u.Name, block); err != nil {
		// Hand the block back rather than leave it recorded against a container
		// that cannot use it.
		if rerr := m.db.UpdateUserIPv6ExtraBlock(u.ID, ""); rerr != nil {
			fmt.Printf("  ! warn: could not undo the /64 assignment for %s: %v\n", u.Name, rerr)
		}
		return "", err
	}
	if err := m.ConfigureContainerIPv6(u.Name, "", block); err != nil {
		fmt.Printf("  ! warn: %s was given %s but the container rejected its IPv6 config: %v\n", u.Name, block, err)
	}
	_ = m.writeNDPPD("", "")
	return block, nil
}
