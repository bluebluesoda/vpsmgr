package mgr

import (
	"fmt"
	"sort"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
	"vpsmgr/internal/su"
)

// PoolIPv6 returns the pool-mode IPv6 address assigned to a user: the stored
// DB value when present, "" when the user has none (V4-only container).
// Pool mode is the ONLY caller of the stored address; prefix mode still
// derives addresses on the fly via IPv6Addr.
func (m *Manager) PoolIPv6(u *db.User) string {
	if u == nil {
		return ""
	}
	return u.IPv6Address
}

// freePoolIPv6s returns the pool addresses that are NOT currently assigned to
// any user, in the pool's configured order. The pool is validated so a
// misconfigured (non-global / duplicate) entry is caught at install time, but
// a hand-edited config is still checked here so a bad entry can never be
// handed to a container.
func (m *Manager) freePoolIPv6s() ([]string, error) {
	pool, err := m.cfg.IPv6PoolValidated()
	if err != nil {
		return nil, err
	}
	if len(pool) == 0 {
		return nil, nil
	}
	used, err := m.db.UsedIPv6Addresses()
	if err != nil {
		return nil, err
	}
	free := make([]string, 0, len(pool))
	for _, a := range pool {
		if !used[a] {
			free = append(free, a)
		}
	}
	return free, nil
}

// pickPoolIPv6 chooses the IPv6 address for a new container in pool mode:
//   - want: the admin's explicit choice ("" = auto)
//   - explicit choice must be in the pool and unused
//   - auto picks the first free address in pool order
//
// Returns "" when the pool is exhausted (a V4-only container is then
// created) or the caller opted out with want="none" (sentinel handled by the
// caller). The chosen address is NOT reserved here — the reservation happens
// atomically in the DB create, so a crash cannot leak an address.
func (m *Manager) pickPoolIPv6(want string) (string, error) {
	pool, err := m.cfg.IPv6PoolValidated()
	if err != nil {
		return "", err
	}
	if len(pool) == 0 {
		return "", nil
	}
	used, err := m.db.UsedIPv6Addresses()
	if err != nil {
		return "", err
	}
	if want != "" {
		if !contains(pool, want) {
			return "", fmt.Errorf("ipv6 %s is not in the configured pool", want)
		}
		if used[want] {
			return "", fmt.Errorf("ipv6 %s is already assigned to another user", want)
		}
		return want, nil
	}
	for _, a := range pool {
		if !used[a] {
			return a, nil
		}
	}
	return "", nil // pool exhausted -> V4-only
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// FreePoolIPv6List returns the currently free pool addresses (used by the
// admin panel's create-user dropdown). Sorted for a stable UI.
func (m *Manager) FreePoolIPv6List() []string {
	free, err := m.freePoolIPv6s()
	if err != nil {
		return nil
	}
	sort.Strings(free)
	return free
}

// IPv6PoolUsage returns the configured pool size and the number of addresses
// currently assigned (for display in the admin panel).
func (m *Manager) IPv6PoolUsage() (total, used int, err error) {
	pool, err := m.cfg.IPv6PoolValidated()
	if err != nil {
		return 0, 0, err
	}
	total = len(pool)
	if total == 0 {
		return 0, 0, nil
	}
	usedSet, err := m.db.UsedIPv6Addresses()
	if err != nil {
		return 0, 0, err
	}
	for _, a := range pool {
		if usedSet[a] {
			used++
		}
	}
	return total, used, nil
}

// IPv6Mode returns the effective IPv6 mode (none|prefix|pool) for the panel /
// CLI.
func (m *Manager) IPv6Mode() string { return m.cfg.IPv6ModeEffective() }

// ModeFor returns the effective IPv6 mode for a config that may be nil
// (unit-test convenience).
func ModeFor(c *cfg.Config) string {
	if c == nil {
		return cfg.IPv6ModeNone
	}
	return c.IPv6ModeEffective()
}

// WireIPv6Pool prepares a pool-mode /128 for a container. The container's
// routed NIC (Incus nictype=routed, parent=ext_if) programs the host route
// + proxy_ndp itself, so the ONLY host-side requirement is that the address
// is NOT bound on the external interface: a whitelist provider assigns the
// address to the host at boot, and while the host holds it the kernel treats
// it as a LOCAL address — the container's packets with that source are then
// dropped as spoofed (source-address validation) and outbound routing fails.
// Removing it from the external interface (idempotent, EADDRNOTAVAIL is
// fine) hands the address exclusively to the container. Called at
// add/reinstall and by RewireAllIPv6Pool (so a reboot that re-binds the
// address on eth0 self-heals on the next reapply).
func (m *Manager) WireIPv6Pool(name, addr string) error {
	if addr == "" {
		return nil
	}
	ext := m.cfg.Net.ExtIF
	if ext == "" {
		return fmt.Errorf("no external interface for pool mode")
	}
	// Best-effort removal: the address may not be on eth0 (already removed).
	_, _ = su.IP6("addr-del", addr, ext)
	return nil
}

// UnwireIPv6Pool is a no-op for pool mode: the address stays free in the DB
// (released on user delete); the host-side routed-NIC plumbing dies with the
// container. Kept for symmetry with prefix mode.
func (m *Manager) UnwireIPv6Pool(name, addr string) error {
	return nil
}

// RewireAllIPv6Pool rebuilds the whole pool-mode host plumbing: ensure every
// pool address is NOT bound on the external interface (see WireIPv6Pool) and
// the external interface has proxy_ndp + forwarding enabled (required by the
// routed NIC). Called at boot (vps-ipv6.service) and by `vps install` /
// `vps ipv6-reapply` so pass-through survives reboots. Idempotent.
func (m *Manager) RewireAllIPv6Pool() error {
	pool, err := m.cfg.IPv6PoolValidated()
	if err != nil {
		return err
	}
	var firstErr error
	for _, a := range pool {
		if err := m.WireIPv6Pool("", a); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if err := m.enableProxyNDP(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := m.enableForwarding(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}
