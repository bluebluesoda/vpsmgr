package fw

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/su"
)

type Firewall struct {
	cfg *cfg.Config
}

func New(c *cfg.Config) *Firewall { return &Firewall{cfg: c} }

func (f *Firewall) userFile(name string) string {
	return filepath.Join(f.cfg.NftDir(), "user-"+name+".nft")
}

func (f *Firewall) MainPath() string { return f.cfg.NftMain() }

// mainContent renders the authoritative main config: delete the table first so
// `nft -f` applies the whole ruleset as ONE atomic batch (if any rule fails,
// the previous table survives instead of vanishing mid-reload).
func mainContent(c *cfg.Config) string {
	sub := c.Net.Subnet
	ext := c.Net.ExtIF
	return fmt.Sprintf(`%sdelete table inet vpsmgr
table inet vpsmgr {
  chain prerouting {
    type nat hook prerouting priority dstnat; policy accept;
  }

  chain output {
    type nat hook output priority dstnat; policy accept;
  }

  chain postrouting {
    type nat hook postrouting priority srcnat; policy accept;
    ip saddr %s oifname "%s" masquerade
  }

  chain forward {
    type filter hook forward priority filter; policy accept;
    # Port 25 (SMTP) is blocked for ALL forwarded traffic, both directions,
    # TCP and UDP — anti-spam. Permanent by design: no user toggle; it only
    # goes away when the panel is uninstalled.
    tcp dport 25 drop
    tcp sport 25 drop
    udp dport 25 drop
    udp sport 25 drop
  }

  chain redirect-drop {
    type filter hook output priority filter; policy accept;
    icmpv6 type nd-redirect drop
  }
}
include "%s"
`, cfg.GeneratedBanner, sub, ext, filepath.Join(c.NftDir(), "*.nft"))
}

// WriteMain writes the authoritative main config (table, chains, masquerade,
// include of per-user files).
func (f *Firewall) WriteMain() error {
	return os.WriteFile(f.MainPath(), []byte(mainContent(f.cfg)), 0o644)
}

// WriteUser writes the DNAT rules for a user. Two sets: prerouting (external
// traffic) and output (connections originating on the host itself, e.g. the
// acceptance test `ssh -p <sshPort> root@<hostIP>`). Output rules are scoped
// to the host's own IP so unrelated local connections are not hijacked.
// sshPort is the user's random SSH port (DNAT to container:22, TCP only);
// startPort..startPort+perUser-1 is the user's whole-hundred block of ports
// forwarded straight to the container (TCP and UDP).
func (f *Firewall) WriteUser(name, ip string, sshPort, startPort, perUser int) error {
	last := startPort + perUser - 1
	daddr := f.cfg.Panel.PublicIP
	if daddr == "" || daddr == "127.0.0.1" {
		daddr = ""
	}
	var b strings.Builder
	b.WriteString(cfg.GeneratedBanner)
	fmt.Fprintf(&b, "add rule inet vpsmgr prerouting tcp dport %d dnat ip to %s:22\n", sshPort, ip)
	fmt.Fprintf(&b, "add rule inet vpsmgr prerouting tcp dport %d-%d dnat ip to %s\n", startPort, last, ip)
	fmt.Fprintf(&b, "add rule inet vpsmgr prerouting udp dport %d-%d dnat ip to %s\n", startPort, last, ip)
	if daddr != "" {
		fmt.Fprintf(&b, "add rule inet vpsmgr output ip daddr %s tcp dport %d dnat ip to %s:22\n", daddr, sshPort, ip)
		fmt.Fprintf(&b, "add rule inet vpsmgr output ip daddr %s tcp dport %d-%d dnat ip to %s\n", daddr, startPort, last, ip)
		fmt.Fprintf(&b, "add rule inet vpsmgr output ip daddr %s udp dport %d-%d dnat ip to %s\n", daddr, startPort, last, ip)
	}
	return os.WriteFile(f.userFile(name), []byte(b.String()), 0o600)
}

func (f *Firewall) RemoveUser(name string) error {
	err := os.Remove(f.userFile(name))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Reload rebuilds the vpsmgr table as one atomic nft batch. nft rules do not
// survive a reboot, so on boot the table does not exist and a `delete table`
// inside the batch would fail it; `nft add table` is idempotent, so ensure the
// table exists first. The batch then delete-and-recreates atomically: any rule
// error rolls the whole batch back and the previous table stays intact.
//
// The config is validated with `nft -c` BEFORE it is applied (the panel
// daemon, an unprivileged user, writes these files — the check keeps a syntax
// or semantic error from ever reaching the live ruleset; combined with the
// atomic batch, a bad generation leaves the previous table running).
//
// nft needs CAP_NET_ADMIN, so the reload runs through the sudoers whitelist
// (the panel daemon is unprivileged; only this exact command is allowed).
func (f *Firewall) Reload() error {
	if _, err := su.Run("/usr/sbin/nft", "-c", "-f", f.MainPath()); err != nil {
		return fmt.Errorf("nft config check failed (not applied): %w", err)
	}
	_, _ = su.Run("/usr/sbin/nft", "add", "table", "inet", "vpsmgr")
	if _, err := su.Run("/usr/sbin/nft", "-f", f.MainPath()); err != nil {
		return err
	}
	return nil
}
