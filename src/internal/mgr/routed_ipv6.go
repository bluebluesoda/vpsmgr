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
//   - RHEL-family (NetworkManager): the connection is set to ipv6.method=ignore
//     (the kernel then handles RA), the kernel sysctls give the RA default
//     route but not the on-link prefix and drop redirects, and the primary
//     /128 is applied via vpsmgr-ipv6.service. The sysctls/helper/unit are
//     baked into the image but are also installed here when an older image
//     lacks them, so a pre-fix image still gets working IPv6.
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
  CONN=$(nmcli -t -f NAME con show 2>/dev/null | grep -i eth0 | head -1)
  [ -n "$CONN" ] && nmcli con mod "$CONN" ipv6.method ignore >/dev/null 2>&1 || true
  printf '%%s/128\n' %q > /etc/vpsmgr-ipv6.conf
  # The RHEL image bakes these, but images published before the fix (or built
  # from an older script) lack them — install idempotently so this always
  # works: kernel takes the RA default route but never the on-link prefix.
  if [ ! -f /etc/sysctl.d/99-vpsmgr-ipv6.conf ]; then
    printf 'net.ipv6.conf.eth0.accept_ra = 1\nnet.ipv6.conf.eth0.accept_ra_pinfo = 0\nnet.ipv6.conf.eth0.accept_redirects = 0\n' > /etc/sysctl.d/99-vpsmgr-ipv6.conf
  fi
  sysctl -p /etc/sysctl.d/99-vpsmgr-ipv6.conf 2>/dev/null || true
  # Fully static IPv6: the panel writes /etc/vpsmgr-ipv6.conf with the
  # deterministic /128. The helper temporarily enables RA processing to learn
  # the gateway from the bridge dnsmasq, then disables RA and pins the address
  # + a static default route via the learned gateway. Waits for DAD to finish
  # (a tentative source breaks the route add) and exits 1 on any failure so
  # the unit (Restart=on-failure) retries. Rewritten on EVERY configure run.
  printf '#!/bin/sh\nfor i in $(seq 1 150); do\n  [ -f /etc/vpsmgr-ipv6.conf ] && break\n  sleep 2\ndone\n[ -f /etc/vpsmgr-ipv6.conf ] || exit 1\nADDR=$(cat /etc/vpsmgr-ipv6.conf)\nADDR_BARE=${ADDR%%/*}\nsysctl -w net.ipv6.conf.eth0.accept_ra=1 >/dev/null 2>&1\nsysctl -w net.ipv6.conf.all.accept_ra=1 >/dev/null 2>&1\nGW=""\nfor i in $(seq 1 60); do\n  GW=$(ip -6 route show dev eth0 | awk "/default/{print \\$3; exit}")\n  [ -n "$GW" ] && break\n  sleep 2\ndone\n[ -n "$GW" ] || exit 1\nsysctl -w net.ipv6.conf.eth0.accept_ra=0 >/dev/null 2>&1\nsysctl -w net.ipv6.conf.all.accept_ra=0 >/dev/null 2>&1\nip -6 addr replace "$ADDR" dev eth0 2>/dev/null\nip -6 addr show dev eth0 scope global | grep inet6 | while read -r line; do\n  a=$(echo "$line" | awk "{print \\$2}" | cut -d/ -f1)\n  [ "$a" != "$ADDR_BARE" ] && ip -6 addr del "$a" dev eth0 2>/dev/null\ndone\n# Wait for DAD to complete (tentative -> valid), then pin the static route.\nfor i in $(seq 1 30); do\n  ip -6 addr show dev eth0 scope global | grep -q tentative || break\n  sleep 1\ndone\nip -6 route flush dev eth0 2>/dev/null\nip -6 route add default via "$GW" dev eth0 src "$ADDR_BARE" || exit 1\nip -6 route flush cache 2>/dev/null\n' > /usr/local/sbin/vpsmgr-ipv6
  chmod +x /usr/local/sbin/vpsmgr-ipv6
  printf '[Unit]\nDescription=vpsmgr IPv6 primary address\nAfter=network-online.target\nWants=network-online.target\n[Service]\nType=oneshot\nExecStart=/usr/local/sbin/vpsmgr-ipv6\nRemainAfterExit=yes\nRestart=on-failure\nRestartSec=10\n[Install]\nWantedBy=multi-user.target\n' > /etc/systemd/system/vpsmgr-ipv6.service
  systemctl daemon-reload >/dev/null 2>&1 || true
  systemctl enable vpsmgr-ipv6.service >/dev/null 2>&1 || true
  # Start the staticize unit in the background: it waits for conf + the RA
  # default route and retries (Restart=on-failure) until it converges, and
  # must not block the panel's configure call.
  systemctl --no-block start vpsmgr-ipv6.service >/dev/null 2>&1 || true
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
