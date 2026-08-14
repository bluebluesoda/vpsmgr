package mgr

import (
	"fmt"
	"strings"
)

// ipv6ContainerScript returns a shell script that configures a container's
// IPv6 so peers are reached through the host instead of direct L2 neighbour
// discovery (which port isolation blocks), and applies the deterministic
// primary /128:
//
//   - Debian (systemd-networkd): rebuild [IPv6AcceptRA] with
//     UseOnLinkPrefix=false + UseRoutePrefix=false + UseAutonomousPrefix=false
//   - DHCPv6Client=no so the parent prefix is not on-link, no SLAAC address
//     is generated, and the RA's Managed flag never starts a DHCPv6 client,
//     bind the deterministic /128 with an [Address] section, and set
//     DHCP=ipv4. Idempotent and self-healing: a mangled config from an older
//     buggy run is stripped and rebuilt.
//   - RHEL-family (NetworkManager): the connection is declared as a STATIC
//     IPv6 profile via nmcli (ipv6.method manual + the deterministic /128 as
//     the address and fe80::1 as the gateway). NetworkManager owns the whole
//     IPv6 stack: it sets accept_ra=0 itself (no kernel RA interference, no
//     SLAAC/dynamic addresses), applies address + default route atomically,
//     and re-applies the profile on every boot. No daemons, no waiting.
//
// Both end by flushing stale on-link / redirect routes for the parent prefix.
func (m *Manager) ipv6ContainerScript(name string) (string, error) {
	ipv6, err := m.IPv6Addr(name)
	if err != nil {
		return "", err
	}
	if ipv6 == "" {
		return "", nil
	}
	n, err := m.cfg.IPv6Network()
	if err != nil {
		return "", err
	}
	// Bare prefix (without the trailing ::) used to match routes that belong
	// to the parent prefix, e.g. 2406:da14:1dd2:a807:753a for a /80.
	prefix := strings.TrimSuffix(n.IP.String(), "::")
	script := fmt.Sprintf(`set -e
# Wait for systemd to be ready before touching it: right after boot the
# /run/systemd/private bus socket may not exist yet, and systemctl then fails
# with "Failed to connect to system scope bus". Bounded, cheap.
for i in $(seq 1 40); do
  [ -S /run/systemd/private ] && break
  sleep 0.5
done
if command -v nmcli >/dev/null 2>&1 && ! systemctl is-active systemd-networkd >/dev/null 2>&1; then
  # RHEL-family (NetworkManager): declare the deterministic /128 and the
  # gateway as a STATIC IPv6 connection. ipv6.method=manual makes NetworkManager
  # own the whole IPv6 stack: it sets accept_ra=0 itself (no kernel RA
  # interference, no SLAAC/dynamic addresses), applies the address and the
  # default route atomically, and re-applies on every boot. No waiting, no
  # retries, no sysctl poking — the connection profile IS the final state.
  CONN=$(nmcli -t -f NAME con show 2>/dev/null | grep -i eth0 | head -1)
  [ -n "$CONN" ] && nmcli con mod "$CONN" \
    ipv6.method manual \
    ipv6.addresses %s/128 \
    ipv6.gateway fe80::1 2>/dev/null || true
  [ -n "$CONN" ] && nmcli con up "$CONN" >/dev/null 2>&1 || true
else
  CFG=/etc/systemd/network/eth0.network
  changed=0
  # Rebuild the [IPv6AcceptRA] section from scratch — the only way to heal
  # both the mangled single-line residue buggy old versions wrote and sections
  # missing any option. Canonical contents: the parent prefix is never on-link
  # and never a route, no SLAAC address is generated, and the RA's Managed flag
  # must not start a DHCPv6 client (a dynamic address would fall outside the
  # routed /112, which ipv6_filtering drops).
  if ! grep -qs '^DHCPv6Client=no$' "$CFG" 2>/dev/null; then
    awk 'BEGIN{s=0} /^n\[IPv6AcceptRA\]/{next} /^\[IPv6AcceptRA\]$/{s=1;next} s&&/^\[/{s=0} !s' "$CFG" > "$CFG.new" && mv "$CFG.new" "$CFG"
    printf '\n[IPv6AcceptRA]\nUseOnLinkPrefix=false\nUseRoutePrefix=false\nUseAutonomousPrefix=false\nDHCPv6Client=no\n' >> "$CFG"
    changed=1
  fi
  # Statically bind the deterministic primary /128. A reinstall deletes the
  # container but Incus's dnsmasq keeps its DHCPv6 lease for up to an hour, so
  # DHCPv6 would hand the recreated container a dynamic address instead —
  # binding the /128 directly makes IPv6 independent of that.
  if ! grep -qs '^Address=%s/128$' "$CFG" 2>/dev/null; then
    printf '\n[Address]\nAddress=%s/128\n' %q >> "$CFG"
    changed=1
  fi
  # Drop DHCPv6: the dynamic address it would assign is outside the /112.
  if grep -qs '^DHCP=true$' "$CFG" 2>/dev/null; then
    sed -i 's/^DHCP=true$/DHCP=ipv4/' "$CFG"
    changed=1
  fi
  if [ "$changed" = 1 ]; then
    systemctl restart systemd-networkd || true
  fi
fi
for r in $(ip -6 route show dev eth0 | awk '{print $1}'); do
  case "$r" in
    %s*) ip -6 route del "$r" dev eth0 2>/dev/null || true ;;
  esac
done
ip -6 route flush cache 2>/dev/null || true`, ipv6, ipv6, ipv6, ipv6, prefix)
	return script, nil
}

// ConfigureContainerIPv6 applies the host-routed IPv6 setup to one container
// (its stack decides the mechanism). Called on add/reinstall for new
// containers and by EnsureRoutedIPv6 for existing ones. No-op when IPv6 is
// disabled or the container has no address.
func (m *Manager) ConfigureContainerIPv6(name string) error {
	if !m.cfg.IPv6Enabled() {
		return nil
	}
	script, err := m.ipv6ContainerScript(name)
	if err != nil || script == "" {
		return err
	}
	if _, err := m.lx.ExecSH(name, script); err != nil {
		return err
	}
	return nil
}

// EnsureRoutedIPv6 applies the host-routed IPv6 setup to every running
// container, so inter-container IPv6 goes through the host with L2 between
// containers staying isolated (no broadcast/NDP plane, no MITM, only
// address-addressed routed traffic). Runs on `vps install` and
// `vps ipv6-reapply`; idempotent. Stopped or not-yet-created containers
// are skipped, not errors.
func (m *Manager) EnsureRoutedIPv6() error {
	if !m.cfg.IPv6Enabled() {
		return nil
	}
	users, err := m.db.ListUsers()
	if err != nil {
		return err
	}
	var firstErr error
	for _, u := range users {
		if st, err := m.lx.State(u.Name); err != nil || st != "Running" {
			continue // stopped or not created yet
		}
		if err := m.ConfigureContainerIPv6(u.Name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
