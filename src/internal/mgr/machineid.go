package mgr

import (
	"fmt"
	"os"
	"strings"
)

// dnsmasqLeasesPath returns the path of the bridge's dnsmasq lease file for
// the Debian-package Incus install vpsmgr targets. IPv4 and DHCPv6 leases
// share this file; a DHCPv6 entry is the one whose third column is an IPv6
// address.
func dnsmasqLeasesPath(bridge string) string {
	return "/var/lib/incus/networks/" + bridge + "/dnsmasq.leases"
}

// EnsureUniqueMachineID repairs containers that share a machine-id baked into
// pre-0.2.5 images. systemd-networkd derives its DHCPv6 DUID from the
// machine-id, so two containers presenting the same DUID make dnsmasq drop
// their DHCPv6 lease at the 1h renewal and the container's global IPv6
// disappears. The repair: give each affected container a fresh machine-id,
// stop them, drop the stale DHCPv6 leases dnsmasq still holds for the old
// DUID, restart the bridge's dnsmasq, and start them again so each re-acquires
// its reserved address. No-op (no churn) when no machine-id is shared.
func (m *Manager) EnsureUniqueMachineID() error {
	if !m.cfg.IPv6Enabled() {
		return nil
	}
	users, err := m.db.ListUsers()
	if err != nil {
		return err
	}
	// 1. Find machine-ids shared by more than one container.
	byID := map[string][]string{}
	for _, u := range users {
		out, err := m.lx.ExecSH(u.Name, "cat /etc/machine-id 2>/dev/null || true")
		if err != nil {
			continue // stopped or not created yet
		}
		if id := strings.TrimSpace(out); id != "" {
			byID[id] = append(byID[id], u.Name)
		}
	}
	var affected []string
	for _, names := range byID {
		if len(names) > 1 {
			affected = append(affected, names...)
		}
	}
	if len(affected) == 0 {
		return nil
	}
	// 2. Fresh machine-id per affected container.
	for _, name := range affected {
		if _, err := m.lx.ExecSH(name, "rm -f /etc/machine-id /var/lib/dbus/machine-id && systemd-machine-id-setup >/dev/null 2>&1"); err != nil {
			return fmt.Errorf("regenerate machine-id in %s: %w", name, err)
		}
	}
	// 3. Stop the affected containers, drop the stale DHCPv6 leases, restart
	//    the bridge dnsmasq, and bring them back up.
	for _, name := range affected {
		if err := m.lx.Stop(name); err != nil {
			return fmt.Errorf("stop %s for machine-id repair: %w", name, err)
		}
	}
	if err := dropDHCPv6Leases(dnsmasqLeasesPath(m.cfg.Incus.Bridge)); err != nil {
		return err
	}
	if err := m.restartBridgeDNS(); err != nil {
		return err
	}
	for _, name := range affected {
		if err := m.lx.Start(name); err != nil {
			return fmt.Errorf("start %s after machine-id repair: %w", name, err)
		}
	}
	return nil
}

// dropDHCPv6Leases removes DHCPv6 entries from the bridge's dnsmasq lease file
// so dnsmasq can hand the reserved addresses to their owners again. A DHCPv6
// entry carries an IPv6 address in the third column (IPv4 lines are dotted,
// the `duid <server>` declaration has no third column). Rewrites the file only
// when something changed; a missing file is fine.
func dropDHCPv6Leases(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	kept := make([]string, 0, 8)
	changed := false
	for _, ln := range strings.Split(string(data), "\n") {
		if f := strings.Fields(ln); len(f) >= 3 && strings.Contains(f[2], ":") {
			changed = true
			continue
		}
		kept = append(kept, ln)
	}
	if !changed {
		return nil
	}
	return os.WriteFile(path, []byte(strings.Join(kept, "\n")), 0o644)
}

// restartBridgeDNS toggles the bridge's stateful DHCP so Incus restarts dnsmasq,
// which re-reads the (now DHCPv6-free) lease file.
func (m *Manager) restartBridgeDNS() error {
	b := m.cfg.Incus.Bridge
	if err := m.lx.NetworkSet(b, "ipv6.dhcp.stateful=false"); err != nil {
		return err
	}
	return m.lx.NetworkSet(b, "ipv6.dhcp.stateful=true")
}
