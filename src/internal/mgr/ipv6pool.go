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

// WireIPv6Pool registers a single pool-mode /128 with the host: a /128 route
// through the Incus bridge (Incus routes the address to the container) and a
// kernel proxy_ndp entry on the external interface (upstream neighbor
// solicitations for the address are answered by the host, which forwards to
// the container). This is the old pre-/112 single-address mechanism, now
// driven by DB-stored pool assignments. Idempotent (EEXIST is fine).
func (m *Manager) WireIPv6Pool(name, addr string) error {
	if addr == "" {
		return nil
	}
	bridge := m.cfg.Incus.Bridge
	ext := m.cfg.Net.ExtIF
	if ext == "" {
		return fmt.Errorf("no external interface for proxy_ndp")
	}
	if _, err := su.IP6("route-add", addr+"/128", bridge); err != nil && !isExistsErr(err) {
		return fmt.Errorf("add /128 route %s dev %s: %w", addr, bridge, err)
	}
	if _, err := su.IP6("neigh-add-proxy", addr, ext); err != nil && !isExistsErr(err) {
		return fmt.Errorf("add proxy_ndp %s dev %s: %w", addr, ext, err)
	}
	return nil
}

// UnwireIPv6Pool removes a pool-mode /128 from the host plumbing. Best-effort:
// a leftover proxy entry is harmless and cleaned by RewireAllIPv6Pool.
func (m *Manager) UnwireIPv6Pool(name, addr string) error {
	if addr == "" {
		return nil
	}
	bridge := m.cfg.Incus.Bridge
	ext := m.cfg.Net.ExtIF
	if ext != "" {
		_, _ = su.IP6("neigh-del-proxy", addr, ext)
	}
	_, _ = su.IP6("route-del", addr, bridge)
	return nil
}

// RewireAllIPv6Pool rebuilds the whole pool-mode plumbing: for every user with
// a stored pool address, re-add the /128 route + proxy_ndp entry. Called at
// boot (vps-ipv6.service) and by `vps install` / `vps ipv6-reapply` so
// pass-through survives reboots. Idempotent.
func (m *Manager) RewireAllIPv6Pool() error {
	users, err := m.db.ListUsers()
	if err != nil {
		return err
	}
	var firstErr error
	for _, u := range users {
		if u.IPv6Address == "" {
			continue
		}
		if err := m.WireIPv6Pool(u.Name, u.IPv6Address); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
