package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"vpsmgr/internal/admin"
	"vpsmgr/internal/cert"
	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
	"vpsmgr/internal/fw"
	"vpsmgr/internal/inter"
	"vpsmgr/internal/mgr"
	"vpsmgr/internal/panel"
	"vpsmgr/internal/pw"
	"vpsmgr/internal/su"
	"vpsmgr/internal/ver"
)

const panelUnit = `# Managed by vpsmgr — installed file, do not edit by hand.
# Changes are overwritten on the next install.
[Unit]
Description=vps panel
After=network-online.target incus.service
Wants=network-online.target
[Service]
Type=simple
# Unprivileged: the panel talks to Incus over its group-readable socket
# (incus-admin) and escalates only the exact whitelisted commands via sudo.
User=vps
Group=vps
ExecStart=/usr/local/bin/vps serve
Restart=always
RestartSec=3
# The panel writes /etc/vpsmgr (config/db/certs) and /etc/traefik/dynamic.
# /etc/vpsmgr is chowned to vps at install; ProtectSystem is off so those
# writes work while the rest of the host stays untouched by policy.
NoNewPrivileges=false
[Install]
WantedBy=multi-user.target
`

const nftUnit = `# Managed by vpsmgr — installed file, do not edit by hand.
# Changes are overwritten on the next install.
[Unit]
Description=vps nftables rules
After=network-online.target incus.service
Wants=network-online.target
Before=vps.service
[Service]
Type=oneshot
# The config file starts with a delete-table line so the whole ruleset reloads
# as ONE atomic batch (a failed rule keeps the previous table intact).
ExecStart=/usr/sbin/nft -f /etc/vpsmgr/nftables.conf
RemainAfterExit=yes
[Install]
WantedBy=multi-user.target
`

const ipv6Unit = `# Managed by vpsmgr — installed file, do not edit by hand.
# Changes are overwritten on the next install.
[Unit]
Description=vps IPv6 pass-through routes
After=network-online.target incus.service vps-nft.service
Wants=network-online.target
Before=vps.service
[Service]
Type=oneshot
ExecStart=/usr/local/bin/vps ipv6-reapply
RemainAfterExit=yes
[Install]
WantedBy=multi-user.target
`

func main() {
	log.SetFlags(0)
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "install":
		err = cmdInstall()
	case "serve":
		err = cmdServe()
	case "panel-url":
		err = cmdPanelURL()
	case "add":
		err = userAdd(os.Args[2:])
	case "del":
		if len(os.Args) != 3 {
			err = fmt.Errorf("usage: vps del <name>")
			break
		}
		err = userDel(os.Args[2])
	case "list":
		// `vps list` = table of all users; `vps list <name>` = one user's detail.
		if len(os.Args) == 2 {
			err = userList()
		} else if len(os.Args) == 3 {
			err = userShow(os.Args[2])
		} else {
			err = fmt.Errorf("usage: vps list [name]")
		}
	case "quota":
		err = userQuota(os.Args[2:])
	case "power":
		if len(os.Args) != 4 {
			err = fmt.Errorf("usage: vps power <name> start|stop|restart")
			break
		}
		err = userPower(os.Args[3], os.Args[2])
	case "passwd":
		if len(os.Args) != 3 {
			err = fmt.Errorf("usage: vps passwd <name>")
			break
		}
		err = userPasswd(os.Args[2])
	case "admin-passwd":
		err = cmdAdminPasswd()
	case "ipv6-reapply":
		// Re-attach IPv6 routes/proxy_ndp for all existing containers.
		// Run by the vps-ipv6.service boot unit and `vps install`.
		err = cmdIPv6Reapply()
	case "note-version":
		if len(os.Args) != 3 {
			err = fmt.Errorf("usage: vps note-version <version>")
			break
		}
		err = cmdNoteVersion(os.Args[2])
	case "config":
		err = cmdConfig(os.Args[2:])
	case "version":
		fmt.Println(ver.Version)
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`vps ` + ver.Version + `
usage:
  vps list [name]                  all users, or one user's detail
  vps add <name> [--cpu 1] [--mem 1G] [--disk 10G] [--bandwidth 100]
  vps quota <name> [--cpu 2] [--mem 2G] [--disk 20G] [--bandwidth 200]
  vps power <name> start|stop|restart
  vps passwd <name>                reissue user panel password (shown once)
  vps admin-passwd                 reset admin panel password (shown once)
  vps del <name>
  vps panel-url                    print panel address
  vps config list|set|help         inspect/change config.yaml (per-field validated edits)
  vps version
system:
  vps install | serve | ipv6-reapply | note-version <ver>
cpu:  whole cores >= 1 (e.g. --cpu 2), or a fraction of one core in 0.1..0.9
      (e.g. --cpu 0.5 — the container is pinned to one core with a time slice)
bandwidth: monthly quota in GiB (upload + download combined); 0 or empty = unlimited
`)
}

func openDB() (*db.DB, error) {
	c, err := cfg.Load()
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	return db.Open(c.Panel.DB)
}

// panelPath returns the secret panel path (e.g. "/Ab1_cdE-9x") or "" if none.
func panelPath(c *cfg.Config) string {
	if c == nil || c.Panel.URLPath == "" {
		return ""
	}
	return "/" + c.Panel.URLPath
}

// blockUnadoptable refuses to run against a config/db that records an origin
// older than this binary can adopt. v0.3 makes breaking changes, so a config
// whose origin is 0.2.x (or older, or not recorded) must not be adopted:
// `vps install` would migrate it, and `vps serve` runs the db migration
// on open — either would corrupt 0.2.x data. Guard both entry points.
//
// The origin is installed_version when set (the last binary to adopt the
// config), falling back to uninstalled_version (a kept config from a
// non-purging uninstall). Only when NEITHER is set is the origin unknown and
// treated as too old. A config that already records 0.3.x must not be blocked
// on an empty counterpart field — that is a normal 0.3.x re-upgrade.
//
// A config whose origin equals this binary's own version is never "older":
// `vps install` records ver.Version as installed_version, so a fresh install
// (and a same-version re-adoption) is always self-signed and safe to run.
// Only a config written by a DIFFERENT, older binary is refused.
func blockUnadoptable(c *cfg.Config) error {
	origin := c.InstalledVersion
	if origin == "" {
		origin = c.UninstalledVersion
	}
	if origin == ver.Version {
		return nil
	}
	if ver.Blocked(origin) {
		return fmt.Errorf("vpsmgr %s cannot adopt a setup from an older release "+
			"(config origin %s): v0.3 makes breaking changes and the migration "+
			"from 0.2.x is not ready yet. Do not upgrade — stay on v0.2.x until "+
			"a migration path exists.",
			ver.Version, orUnknown(origin))
	}
	return nil
}

func orUnknown(v string) string {
	if v == "" {
		return "(unknown)"
	}
	return v
}

func cmdInstall() error {
	c := cfg.Default()
	adopting := false
	if _, err := os.Stat(cfg.Path()); err == nil {
		c, err = cfg.Load()
		if err != nil {
			return err
		}
		adopting = true
	} else {
		if err := c.FillAuto(); err != nil {
			return err
		}
		// Fresh install: generate both secret paths (user 10 / admin 12).
		// After this, an empty path is a deliberate "panel disabled" choice.
		c.EnsurePaths()
		// Fresh install: pick a random free panel port in 2000-9999 instead of
		// the fixed 8443. Best-effort — if every random pick is busy (very
		// unlikely on a fresh host) the code-level default is kept.
		if p, err := cfg.RandomPanelPort(); err == nil {
			c.Panel.Listen = fmt.Sprintf(":%d", p)
		}
	}
	// Adopting an existing config: refuse to touch one that came from a release
	// too old to migrate. Check BEFORE overwriting installed_version below.
	if adopting {
		if err := blockUnadoptable(c); err != nil {
			return err
		}
	}
	// Record which binary version installed (or adopted/upgraded) this config so
	// a future release that makes breaking changes can detect the version the
	// config/db came from and migrate or warn instead of corrupting user data.
	c.InstalledVersion = ver.Version
	if err := cfg.Save(c); err != nil {
		return err
	}
	if err := c.ValidatePaths(); err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DefaultDataDir, 0o755); err != nil {
		return err
	}
	// Incus's security.ipv6_filtering (enabled on every container's eth0) only
	// works while the br_netfilter kernel module is loaded, and Incus does NOT
	// load it itself: a container with the option simply refuses to boot
	// without it. Load it BEFORE any container is created/hardened, and persist
	// it in /etc/modules-load.d so it is present at boot, before Incus starts any
	// container. Harmless no-op where the module is built into the kernel.
	if err := os.MkdirAll("/etc/modules-load.d", 0o755); err != nil {
		return err
	}
	if err := os.WriteFile("/etc/modules-load.d/br_netfilter.conf", []byte("br_netfilter\n"), 0o644); err != nil {
		return err
	}
	_ = exec.Command("modprobe", "br_netfilter").Run()
	if err := os.MkdirAll(cfg.DefaultNftDir, 0o755); err != nil {
		return err
	}
	if err := cert.Ensure(c.Panel.Cert, c.Panel.Key, c.Panel.PublicIP); err != nil {
		return err
	}
	if _, err := os.Stat(c.Panel.DB); err != nil {
		d, err := db.Open(c.Panel.DB)
		if err != nil {
			return err
		}
		d.Close()
	}
	// IPv6 pass-through: configure bridge + re-attach routes for existing
	// containers (no-op when ipv6_subnet empty).
	d, err := db.Open(c.Panel.DB)
	if err != nil {
		return err
	}
	// Immutable fields are enforced from the DB snapshot: refuse a config that
	// drifted from install (first install / pre-snapshot upgrade freezes the
	// current values as the baseline). Done before any Incus/nft mutation.
	snap, ok, err := d.GetSetting(db.SettingImmutableSnapshot)
	if err != nil {
		d.Close()
		return err
	}
	if ok {
		if err := c.VerifyImmutable(snap); err != nil {
			d.Close()
			return err
		}
	} else if snap, err := c.ImmutableSnapshot(); err != nil {
		d.Close()
		return err
	} else if err := d.SetSetting(db.SettingImmutableSnapshot, snap); err != nil {
		d.Close()
		return err
	}
	m := mgr.New(c, d)
	if err := m.RewireAllIPv6(); err != nil {
		d.Close()
		return fmt.Errorf("setup ipv6: %w", err)
	}
	// Container isolation: harden any containers created before the isolated
	// build so the whole fleet is on the same security posture.
	if err := m.HardenAll(); err != nil {
		d.Close()
		return fmt.Errorf("harden containers: %w", err)
	}
	// /112 blocks: add ipv6.routes to containers created before the /112
	// scheme, so each container's whole block is routed to it.
	if err := m.EnsureBlockRoutes(); err != nil {
		d.Close()
		return fmt.Errorf("add ipv6 routes: %w", err)
	}
	// Repair containers sharing a baked-in machine-id (shared DHCPv6 DUID
	// breaks lease renewals and drops the global IPv6 at the 1h mark).
	if err := m.EnsureUniqueMachineID(); err != nil {
		d.Close()
		return fmt.Errorf("unique machine-id: %w", err)
	}
	// Route inter-container IPv6 through the host (no L2 discovery / MITM),
	// so a container can reach a peer whose address it knows.
	if err := m.EnsureRoutedIPv6(); err != nil {
		d.Close()
		return fmt.Errorf("routed ipv6: %w", err)
	}
	d.Close()
	// Unprivileged panel: create the 'vps' service user (if missing), hand it
	// the writable dirs (/etc/vpsmgr, /etc/traefik/dynamic) and the sudoers
	// whitelist, and add it to incus-admin so the socket API is fully usable
	// without root. Idempotent on adoption.
	ensureVPSUser(c)
	f := fw.New(c)
	if err := f.WriteMain(); err != nil {
		return err
	}
	if err := f.Reload(); err != nil {
		return err
	}
	if err := writeUnit("vps.service", panelUnit); err != nil {
		return err
	}
	if err := writeUnit("vps-nft.service", nftUnit); err != nil {
		return err
	}
	// IPv6 pass-through boot unit (re-applies routes/proxy after reboot).
	if c.IPv6Enabled() {
		if err := writeUnit("vps-ipv6.service", ipv6Unit); err != nil {
			return err
		}
	}
	if out, err := exec.Command("systemctl", "daemon-reload").CombinedOutput(); err != nil {
		return fmt.Errorf("daemon-reload: %s", strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("systemctl", "enable", "--now", "vps-nft.service").CombinedOutput(); err != nil {
		return fmt.Errorf("enable vps-nft: %s", strings.TrimSpace(string(out)))
	}
	if c.IPv6Enabled() {
		if out, err := exec.Command("systemctl", "enable", "--now", "vps-ipv6.service").CombinedOutput(); err != nil {
			return fmt.Errorf("enable vps-ipv6: %s", strings.TrimSpace(string(out)))
		}
	}
	if out, err := exec.Command("systemctl", "enable", "--now", "vps.service").CombinedOutput(); err != nil {
		return fmt.Errorf("enable vps: %s", strings.TrimSpace(string(out)))
	}
	// Enforce the IPv4 inbound policy: per-user DNAT rules + traefik state
	// (v4_forward off = IPv6-only, traefik disabled). Idempotent.
	d2, err := db.Open(c.Panel.DB)
	if err != nil {
		return err
	}
	defer d2.Close()
	m2 := mgr.New(c, d2)
	if err := m2.ApplyV4State(); err != nil {
		return fmt.Errorf("apply v4 policy: %w", err)
	}
	// Admin password now lives in the DB (settings table). Migrate a legacy
	// hash found in the config file once, then drop it from the file so the
	// credential is no longer a hand-editable config field.
	if c.Panel.AdminPass != "" {
		if _, ok, err := d2.GetSetting(db.SettingAdminPassHash); err != nil {
			return err
		} else if !ok {
			if err := d2.SetSetting(db.SettingAdminPassHash, c.Panel.AdminPass); err != nil {
				return err
			}
			log.Printf("migrated admin password hash from config.yaml to database")
		}
		c.Panel.AdminPass = ""
		if err := cfg.Save(c); err != nil {
			return err
		}
	}
	// Admin panel: on a FRESH install (admin enabled and no hash yet in the
	// DB) generate a random admin password and show it once. On adoption the
	// existing hash is kept — never reprint a password the admin may not
	// expect to be displayed. When admin is disabled nothing is printed.
	if c.Panel.AdminPath != "" {
		if _, ok, err := d2.GetSetting(db.SettingAdminPassHash); err != nil {
			return err
		} else if !ok {
			pass := pw.Generate(20)
			hash, err := pw.Hash(pass)
			if err != nil {
				return err
			}
			if err := d2.SetSetting(db.SettingAdminPassHash, hash); err != nil {
				return err
			}
			fmt.Printf("admin panel initialized: %s\n", c.PanelURL("/"+c.Panel.AdminPath))
			fmt.Printf("admin password (shown once): %s\n", pass)
		}
	}
	if c.Panel.URLPath != "" {
		fmt.Printf("panel initialized: %s\n", c.PanelURL(panelPath(c)))
	}
	return nil
}

// cmdIPv6Reapply re-attaches IPv6 pass-through plumbing for all existing
// containers (bridge config, /128 routes, proxy_ndp) and re-applies the
// per-container routed-IPv6 config. No-op when IPv6 disabled.
func cmdIPv6Reapply() error {
	c, err := cfg.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	d, err := db.Open(c.Panel.DB)
	if err != nil {
		return err
	}
	defer d.Close()
	m := mgr.New(c, d)
	if err := m.RewireAllIPv6(); err != nil {
		return err
	}
	// Also re-apply the per-container routed-IPv6 config, so the boot unit and
	// a manual `vps ipv6-reapply` heal containers that were created before
	// the host-routed scheme existed or whose networkd config got corrupted.
	return m.EnsureRoutedIPv6()
}

func writeUnit(name, content string) error {
	p := filepath.Join("/etc/systemd/system", name)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ensureVPSUser sets up the unprivileged 'vps' account the panel daemon runs
// as. Idempotent, called by `vps install`:
//   - creates the system user if missing
//   - chowns /etc/vpsmgr and /etc/traefik/dynamic so the panel can write its
//     config, db, certs and domain files without root
//   - adds vps to the incus-admin group so the Incus Unix-socket API is fully
//     usable (the socket is group-rw by incus-admin)
//   - installs the sudoers whitelist granting ONLY the exact privileged
//     commands the panel needs (nft reload, traefik/self systemctl, IPv6
//     route/neigh/addr, sysctl forwarding, ndppd control)
func ensureVPSUser(c *cfg.Config) {
	// 1. System user.
	if out, err := exec.Command("id", "-u", "vps").CombinedOutput(); err != nil {
		_ = exec.Command("useradd", "--system", "--no-create-home", "--home-dir",
			"/nonexistent", "--shell", "/usr/sbin/nologin", "vps").Run()
		_ = out
	}
	// 2. Writable dirs (best-effort; a fresh install created them above).
	_ = exec.Command("chown", "-R", "vps:vps", c.DataDir()).Run()
	_ = exec.Command("chown", "-R", "vps:vps", c.TraefikDir()).Run()
	_ = os.MkdirAll(c.TraefikDir(), 0o755)
	_ = exec.Command("chown", "vps:vps", c.TraefikDir()).Run()
	// 3. incus-admin group membership (the Incus socket's owning group).
	_ = exec.Command("usermod", "-aG", "incus-admin", "vps").Run()
	// 4. sudoers whitelist.
	ensureSudoers()
}

// ensureSudoers installs the panel's sudoers whitelist. The file is embedded
// here (rather than read from disk) so `vps install` is self-contained after
// the binary is in place.
func ensureSudoers() {
	const sudoers = `# Managed by vpsmgr — installed file, do not edit by hand.
# Changes are overwritten on the next install.
#
# The vps panel runs as the unprivileged 'vps' user. These are the ONLY
# root-privileged commands it may run, each pinned to its exact invocation.
vps ALL=(root) NOPASSWD: /usr/sbin/nft add table inet vpsmgr
vps ALL=(root) NOPASSWD: /usr/sbin/nft -f /etc/vpsmgr/nftables.conf
vps ALL=(root) NOPASSWD: /usr/bin/systemctl enable traefik.service
vps ALL=(root) NOPASSWD: /usr/bin/systemctl disable traefik.service
vps ALL=(root) NOPASSWD: /usr/bin/systemctl start traefik.service
vps ALL=(root) NOPASSWD: /usr/bin/systemctl stop traefik.service
vps ALL=(root) NOPASSWD: /usr/bin/systemctl is-active traefik.service
vps ALL=(root) NOPASSWD: /usr/bin/systemctl restart vps.service
vps ALL=(root) NOPASSWD: /usr/bin/systemctl is-active vps.service
vps ALL=(root) NOPASSWD: /sbin/ip -6 route del *
vps ALL=(root) NOPASSWD: /sbin/ip -6 route add *
vps ALL=(root) NOPASSWD: /sbin/ip -6 route replace *
vps ALL=(root) NOPASSWD: /sbin/ip -6 neigh del proxy *
vps ALL=(root) NOPASSWD: /sbin/ip -6 neigh add proxy *
vps ALL=(root) NOPASSWD: /sbin/ip -6 addr replace *
vps ALL=(root) NOPASSWD: /sbin/sysctl -w net.ipv6.conf.all.forwarding=1
vps ALL=(root) NOPASSWD: /usr/sbin/service ndppd restart
vps ALL=(root) NOPASSWD: /usr/sbin/service ndppd start
vps ALL=(root) NOPASSWD: /usr/sbin/service ndppd stop
vps ALL=(root) NOPASSWD: /usr/bin/pkill -x ndppd
vps ALL=(root) NOPASSWD: /usr/sbin/ndppd -d -p /var/run/ndppd.pid
`
	if err := os.MkdirAll("/etc/sudoers.d", 0o750); err != nil {
		return
	}
	tmp := "/etc/sudoers.d/vps.tmp"
	if err := os.WriteFile(tmp, []byte(sudoers), 0o440); err != nil {
		return
	}
	// Validate with visudo -c before activating; a syntax error must never
	// brick sudo for the whole host.
	if out, err := exec.Command("visudo", "-c", "-f", tmp).CombinedOutput(); err != nil {
		_ = os.Remove(tmp)
		log.Printf("warn: sudoers whitelist not installed (%s)", strings.TrimSpace(string(out)))
		return
	}
	_ = os.Rename(tmp, "/etc/sudoers.d/vps")
	_ = exec.Command("chown", "root:root", "/etc/sudoers.d/vps").Run()
	_ = exec.Command("chmod", "0440", "/etc/sudoers.d/vps").Run()
}

func cmdServe() error {
	c, err := cfg.Load()
	if err != nil {
		return err
	}
	if err := blockUnadoptable(c); err != nil {
		return err
	}
	if err := c.ValidatePaths(); err != nil {
		return fmt.Errorf("invalid panel paths: %w", err)
	}
	// Empty path = that panel is disabled. When BOTH are empty the panel
	// service is intentionally off: do not even listen on the port.
	if c.Panel.URLPath == "" && c.Panel.AdminPath == "" {
		log.Printf("both url_path and admin_url_path are empty — panel disabled, not listening on %s", c.Panel.Listen)
		return nil
	}
	d, err := db.Open(c.Panel.DB)
	if err != nil {
		return err
	}
	defer d.Close()
	m := mgr.New(c, d)
	userPath := panelPath(c)
	adminPath := "/" + c.Panel.AdminPath
	var userSrv, adminSrv http.Handler
	if userPath != "" {
		userSrv = panel.New(c, d, m).Handler()
	}
	if c.Panel.AdminPath != "" {
		adminSrv = admin.New(c, d, m).Handler()
	}
	dispatch := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case userSrv != nil && pathUnder(r.URL.Path, userPath):
			userSrv.ServeHTTP(w, r)
		case adminSrv != nil && pathUnder(r.URL.Path, adminPath):
			adminSrv.ServeHTTP(w, r)
		default:
			// No matching enabled panel: bare 404, no body, no headers, no
			// fingerprint.
			w.WriteHeader(http.StatusNotFound)
		}
	})
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	go sampleTrafficLoop(m)
	log.Printf("panel listening on %s (https, self-signed)", c.Panel.Listen)
	return startTLS(c, dispatch, tlsCfg)
}

// pathUnder reports whether path equals prefix or starts with prefix+"/".
func pathUnder(path, prefix string) bool {
	if prefix == "" || prefix == "/" {
		return false
	}
	if path == prefix {
		return true
	}
	return strings.HasPrefix(path, prefix+"/")
}

// sampleTrafficLoop runs the monthly traffic collector until the process ends.
// After every sample it enforces traffic quotas: users over their monthly
// limit get their NIC rate-limited to 1Mbps, users back under (e.g. monthly
// rollover) get the limit removed. Incus applies the NIC limits live (tc), so no
// container restart is involved.
func sampleTrafficLoop(m *mgr.Manager) {
	if err := m.SampleTraffic(); err != nil {
		log.Printf("traffic sample: %v", err)
	}
	if err := m.EnforceTrafficLimits(); err != nil {
		log.Printf("traffic limits: %v", err)
	}
	tick := time.NewTicker(mgr.TrafficInterval)
	defer tick.Stop()
	for range tick.C {
		if err := m.SampleTraffic(); err != nil {
			log.Printf("traffic sample: %v", err)
		}
		if err := m.EnforceTrafficLimits(); err != nil {
			log.Printf("traffic limits: %v", err)
		}
	}
}

func startTLS(c *cfg.Config, h http.Handler, tlsCfg *tls.Config) error {
	srv := &http.Server{Addr: c.Panel.Listen, Handler: h, TLSConfig: tlsCfg}
	return srv.ListenAndServeTLS(c.Panel.Cert, c.Panel.Key)
}

// cmdNoteVersion records the version of the binary that is being uninstalled.
// uninstall.sh calls it with the running binary's version BEFORE removing the
// binary, so a config kept by a non-purging uninstall remembers which version it
// came from. A future release that makes breaking changes can use this to warn
// or migrate instead of failing on incompatible data.
func cmdNoteVersion(v string) error {
	if v == "" {
		return fmt.Errorf("version is empty")
	}
	c, err := cfg.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	c.UninstalledVersion = v
	if err := cfg.Save(c); err != nil {
		return err
	}
	fmt.Printf("recorded version %s (last uninstalled)\n", v)
	return nil
}

func cmdPanelURL() error {
	c, err := cfg.Load()
	if err != nil {
		return err
	}
	if c.Panel.URLPath != "" {
		fmt.Printf("user panel:  %s\n", c.PanelURL(panelPath(c)))
	}
	if c.Panel.AdminPath != "" {
		fmt.Printf("admin panel: %s\n", c.PanelURL("/"+c.Panel.AdminPath))
	}
	if c.Panel.URLPath == "" && c.Panel.AdminPath == "" {
		fmt.Println("both panels are disabled (url_path and admin_url_path are empty)")
	}
	return nil
}

// cmdConfig implements `vps config list|set|help`, the sanctioned interface for
// editing config.yaml. Every key is described by the cfg.Field registry, so the
// CLI enforces the documented kind/apply semantics instead of leaving a raw
// YAML edit as the only path.
func cmdConfig(args []string) error {
	if len(args) == 0 {
		configUsage()
		return nil
	}
	switch args[0] {
	case "list":
		return configList()
	case "set":
		return configSet(args[1:])
	case "help":
		configHelp()
		return nil
	default:
		configUsage()
		return nil
	}
}

func configList() error {
	c, err := cfg.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "KEY\tEDITABLE\tKIND\tVALUE")
	for _, f := range cfg.Fields {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", f.Key, f.Editable(), f.Kind, cfg.FieldValue(c, f.Key))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Println("\neditable: yes = changeable · no = refused · only-when-empty = re-enable a disabled panel")
	return nil
}

func configHelp() {
	w := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	fmt.Fprintln(w, "KEY\tEDITABLE\tKIND\tDESCRIPTION")
	for _, f := range cfg.Fields {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", f.Key, f.Editable(), f.Kind, f.Desc)
	}
	_ = w.Flush()
	fmt.Println("\neditable: yes = changeable · no = refused · only-when-empty = re-enable a disabled panel")
	fmt.Println("applies: restart panel (auto) = applied by vps config set · re-run vps install = needs --apply · immediate = applied right now · next add / reinstall = used later")
}

func configUsage() {
	fmt.Print(`usage:
  vps config list                show current config, annotated with editable/kind/value
  vps config set <key> [<value>] change one field (validated; immutable fields refused;
                                 missing value prompts interactively with an example)
                                 [--apply]    apply install-class fields now (runs vps install)
                                 [--no-apply] save only, do not apply
  vps config help                describe every field and how changes take effect
`)
}

// configSet parses `set <key> <value> [flags]`, validates against the registry,
// saves, and applies the change: restart-class fields restart the panel
// automatically, runtime toggles apply immediately, install-class fields warn
// unless --apply is given.
func configSet(args []string) error {
	key, value := "", ""
	apply, noApply := false, false
	for _, a := range args {
		switch a {
		case "--apply":
			apply = true
		case "--no-apply":
			noApply = true
		default:
			if key == "" {
				key = a
			} else if value == "" {
				value = a
			} else {
				return fmt.Errorf("usage: vps config set <key> <value> [--apply|--no-apply]")
			}
		}
	}
	if key == "" {
		return fmt.Errorf("usage: vps config set <key> <value> [--apply|--no-apply]")
	}
	if apply && noApply {
		return fmt.Errorf("--apply and --no-apply are mutually exclusive")
	}

	f := cfg.FieldFor(key)
	if f == nil {
		return fmt.Errorf("unknown config key %q — see `vps config help`", key)
	}
	c, err := cfg.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	// Immutable fields are refused, except (re-)enabling the user panel path
	// while it is currently empty (panel disabled).
	if f.Kind == cfg.KindImmutable && (f.Editable() != "only-when-empty" || cfg.FieldValue(c, key) != "") {
		cur := cfg.FieldValue(c, key)
		if f.Editable() == "only-when-empty" && cur != "" {
			return fmt.Errorf("%s is not editable (already set to %q) — fixed once set; see `vps config help`", key, cur)
		}
		return fmt.Errorf("%s is not editable (fixed at install) — see `vps config help`", key)
	}
	if f.Kind == cfg.KindAuto {
		return fmt.Errorf("%s is not editable (auto-written by the panel)", key)
	}

	// No value given: prompt interactively with the field description, current
	// value and an example, validating each attempt. Empty input keeps the
	// current value (no change).
	if value == "" {
		if !inter.IsTTY() {
			return fmt.Errorf("usage: vps config set %s <value> [--apply|--no-apply]", key)
		}
		var changed bool
		if value, changed, err = promptConfigValue(c, f); err != nil {
			return err
		}
		if !changed {
			return nil
		}
	}

	old := cfg.FieldValue(c, key)
	if err := f.Assign(c, value); err != nil {
		return err
	}
	if cfg.FieldValue(c, key) == old {
		fmt.Printf("%s is already %q — nothing to do.\n", key, old)
		return nil
	}
	// A change whose apply is disruptive (re-runs `vps install`, or a live
	// runtime toggle) gets a second y/N confirmation before anything is saved.
	if !noApply && dangerousApply(f) && inter.IsTTY() {
		ok, err := confirmApply(c, f, key)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("aborted — %s left unchanged", key)
		}
	}
	if err := cfg.Save(c); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	if noApply {
		fmt.Printf("%s saved (not applied).\n", key)
		return nil
	}

	switch f.Apply {
	case cfg.ApplyRestart:
		switch key {
		case "panel.listen":
			if !listenFree(c.Panel.Listen) {
				return fmt.Errorf("config saved, but the new listen port is already in use (%s) — panel not restarted", c.Panel.Listen)
			}
		case "panel.cert":
			if _, err := os.Stat(c.Panel.Cert); err != nil {
				fmt.Printf("warning: certificate file %s does not exist — panel may fail to start\n", c.Panel.Cert)
			}
		case "panel.key":
			if _, err := os.Stat(c.Panel.Key); err != nil {
				fmt.Printf("warning: key file %s does not exist — panel may fail to start\n", c.Panel.Key)
			}
		}
		if err := restartPanel(); err != nil {
			fmt.Printf("%s saved, but the panel was NOT restarted: %v\n", key, err)
			return nil
		}
		fmt.Printf("%s updated and panel restarted.\n", key)
	case cfg.ApplyInstall:
		if key == "net.ext_if" {
			if _, err := os.Stat("/sys/class/net/" + c.Net.ExtIF); err != nil {
				fmt.Printf("warning: network interface %q not found\n", c.Net.ExtIF)
			}
		}
		if !apply {
			fmt.Printf("%s saved but NOT applied. Run `vps install` to apply (regenerates firewall/routing/container wiring), or re-run with --apply.\n", key)
			return nil
		}
		if err := cmdInstall(); err != nil {
			return fmt.Errorf("config saved, but `vps install` failed: %w", err)
		}
		fmt.Printf("%s updated and applied (vps install ran).\n", key)
	case cfg.ApplyNextAdd:
		fmt.Printf("%s saved. Applies on the next vps add / reinstall.\n", key)
	case cfg.ApplyImmediate:
		if err := applyV4State(c); err != nil {
			return err
		}
		fmt.Printf("%s updated and applied (firewall/traefik state refreshed).\n", key)
	case cfg.ApplyNone:
		return fmt.Errorf("%s is not settable", key)
	}
	return nil
}

// promptConfigValue interactively asks for a new value when
// `vps config set <key>` was run without one. It shows the field description,
// current value and an example, and validates every attempt against the
// field's own Assign rules (re-prompting on error). changed=false means the
// user kept the current value (or, for panel.admin_url_path, typed CLEAR to
// disable — returned as value "" with changed=true).
func promptConfigValue(c *cfg.Config, f *cfg.Field) (value string, changed bool, err error) {
	cur := cfg.FieldValue(c, f.Key)
	def := cur
	if def == "" {
		def = "disabled"
	}
	fmt.Printf("== %s ==\n%s\n", f.Key, f.Desc)
	if f.Key == "panel.admin_url_path" && def == "disabled" {
		fmt.Println("hint: an empty value disables the admin panel; type CLEAR to disable")
	}
	fmt.Printf("current: %s\n", def)
	if f.Example != "" {
		fmt.Printf("example: %s\n", f.Example)
	}
	s, err := inter.Ask("new value", def, "", func(v string) error {
		v = strings.TrimSpace(v)
		if f.Key == "panel.admin_url_path" && strings.EqualFold(v, "clear") {
			return nil
		}
		return cfg.AssignCheck(c, f, v)
	})
	if err != nil {
		return "", false, err
	}
	s = strings.TrimSpace(s)
	if f.Key == "panel.admin_url_path" && strings.EqualFold(s, "clear") {
		return "", true, nil
	}
	if s == def {
		return "", false, nil
	}
	return s, true, nil
}

// dangerousApply reports whether applying a field change is disruptive enough
// to warrant a second confirmation: re-running `vps install` (regenerates
// firewall/routing/container wiring) or a live runtime toggle (net.v4_forward
// changes SSH/port/domain reachability of every container).
func dangerousApply(f *cfg.Field) bool {
	return f.Apply == cfg.ApplyInstall || f.Apply == cfg.ApplyImmediate
}

// confirmApply asks for a y/N confirmation before saving a change whose apply
// is disruptive, showing the new value and what applying does.
func confirmApply(c *cfg.Config, f *cfg.Field, key string) (bool, error) {
	var what string
	switch f.Apply {
	case cfg.ApplyInstall:
		what = "re-runs `vps install`, regenerating the firewall / routing / container wiring"
	case cfg.ApplyImmediate:
		if key == "net.v4_forward" {
			what = "toggles container IPv4 inbound immediately — SSH / port / domain reachability of every container changes"
		} else {
			what = "applies immediately"
		}
	}
	return inter.Confirm(fmt.Sprintf("set %s to %q — this %s. continue?", key, cfg.FieldValue(c, key), what), false)
}

// restartPanel restarts the vps systemd service and waits for it to come
// back. The panel runs unprivileged, so the restart goes through the sudoers
// whitelist. It refuses (returns an error) when the panel is not running as an
// active systemd service — in that case the operator must restart it manually.
func restartPanel() error {
	if _, err := su.Run("/usr/bin/systemctl", "is-active", "--quiet", "vps.service"); err != nil {
		return fmt.Errorf("vps.service is not an active systemd service (%v) — restart the panel manually", err)
	}
	if _, err := su.Run("/usr/bin/systemctl", "restart", "vps.service"); err != nil {
		return fmt.Errorf("systemctl restart vps failed: %w", err)
	}
	for i := 0; i < 20; i++ {
		if _, err := su.Run("/usr/bin/systemctl", "is-active", "--quiet", "vps.service"); err == nil {
			return nil
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("panel did not come back up after restart — check vps.service")
}

// listenFree reports whether addr (e.g. ":8443") is free to bind.
func listenFree(addr string) bool {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// applyV4State enforces the current net.v4_forward policy (firewall + traefik).
func applyV4State(c *cfg.Config) error {
	d, err := db.Open(c.Panel.DB)
	if err != nil {
		return err
	}
	defer d.Close()
	return mgr.New(c, d).ApplyV4State()
}

// cmdAdminPasswd resets the admin panel password to a new random 20-char value
// and prints it once. The password is stored as a bcrypt hash in the config.
func cmdAdminPasswd() error {
	c, err := cfg.Load()
	if err != nil {
		return err
	}
	if c.Panel.AdminPath == "" {
		return fmt.Errorf("admin panel is disabled (admin_url_path is empty) — set it in %s to enable", cfg.Path())
	}
	pass := pw.Generate(20)
	hash, err := pw.Hash(pass)
	if err != nil {
		return err
	}
	d, err := db.Open(c.Panel.DB)
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.SetSetting(db.SettingAdminPassHash, hash); err != nil {
		return err
	}
	fmt.Printf("admin password reset: %s\n", pass)
	fmt.Printf("admin panel: %s\n", c.PanelURL("/"+c.Panel.AdminPath))
	return nil
}

func userAdd(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: vps add <name> [--cpu 1] [--mem 1G] [--disk 10G]")
	}
	name := args[0]
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var cpuS string
	var memS, diskS, trafficS string
	fs.StringVar(&cpuS, "cpu", "", "")
	fs.StringVar(&memS, "mem", "", "")
	fs.StringVar(&diskS, "disk", "", "")
	fs.StringVar(&trafficS, "bandwidth", "", "") // GiB/month, 0/empty = unlimited
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := mgr.ValidateName(name); err != nil {
		return err
	}
	provided := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { provided[f.Name] = true })

	cpu, mem, disk := 10, 1024, 10 // tenths of a core, MiB, GiB
	setCpu, setMem, setDisk := provided["cpu"], provided["mem"], provided["disk"]
	var err error
	if setCpu {
		if cpu, err = mgr.ParseCPU(cpuS); err != nil {
			return err
		}
	}
	if setMem {
		if mem, err = parseMemStrict(memS); err != nil {
			return err
		}
	}
	if setDisk {
		if disk, err = parseDiskStrict(diskS); err != nil {
			return err
		}
	}

	traffic := 0
	if setTraffic := provided["bandwidth"]; setTraffic {
		if traffic, err = mgr.ParseTrafficGB(trafficS); err != nil {
			return err
		}
	}

	if inter.IsTTY() {
		if !setCpu {
			s, err := inter.Ask("CPU cores", "1", "", validateCPU)
			if err != nil {
				return err
			}
			cpu, _ = mgr.ParseCPU(s)
		}
		if !setMem {
			s, err := inter.Ask("Memory", "1024", " MiB (e.g. 512 or 1G)", validateMem)
			if err != nil {
				return err
			}
			mem, _ = parseMemStrict(s)
		}
		if !setDisk {
			s, err := inter.Ask("Disk", "10", " GiB", validateDisk)
			if err != nil {
				return err
			}
			disk, _ = parseDiskStrict(s)
		}
		if !provided["bandwidth"] {
			s, err := inter.Ask("Bandwidth quota", "0", " GiB/month (0 = unlimited)", validateTrafficGB)
			if err != nil {
				return err
			}
			traffic, _ = mgr.ParseTrafficGB(s)
		}
	}

	c, err := cfg.Load()
	if err != nil {
		return err
	}
	d, err := db.Open(c.Panel.DB)
	if err != nil {
		return err
	}
	defer d.Close()
	m := mgr.New(c, d)
	res, err := m.Add(name, mgr.AddOptions{CPU: cpu, MemMB: mem, DiskGB: disk, TrafficGB: traffic})
	if err != nil {
		return err
	}
	printAdded(res)
	return nil
}

func userDel(name string) error {
	c, err := cfg.Load()
	if err != nil {
		return err
	}
	d, err := db.Open(c.Panel.DB)
	if err != nil {
		return err
	}
	defer d.Close()
	m := mgr.New(c, d)
	if err := m.Del(name); err != nil {
		return err
	}
	fmt.Printf("user %s deleted (container, nft rules, traefik config, records)\n", name)
	return nil
}

func userQuota(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: vps quota <name> [--cpu 2] [--mem 2G] [--disk 20G] [--bandwidth 100]")
	}
	name := args[0]
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var cpuS string
	var memS, diskS, trafficS string
	fs.StringVar(&cpuS, "cpu", "", "")
	fs.StringVar(&memS, "mem", "", "")
	fs.StringVar(&diskS, "disk", "", "")
	fs.StringVar(&trafficS, "bandwidth", "", "") // GiB/month, 0 = unlimited
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if err := mgr.ValidateName(name); err != nil {
		return err
	}
	provided := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { provided[f.Name] = true })

	c, err := cfg.Load()
	if err != nil {
		return err
	}
	d, err := db.Open(c.Panel.DB)
	if err != nil {
		return err
	}
	defer d.Close()
	u, err := d.GetUserByName(name)
	if err != nil {
		return err
	}

	cpu, mem, disk := u.CPU, u.MemMB, u.DiskGB
	setCpu, setMem, setDisk := provided["cpu"], provided["mem"], provided["disk"]
	if setCpu {
		if cpu, err = mgr.ParseCPU(cpuS); err != nil {
			return err
		}
	}
	if setMem {
		if mem, err = parseMemStrict(memS); err != nil {
			return err
		}
	}
	if setDisk {
		if disk, err = parseDiskStrict(diskS); err != nil {
			return err
		}
	}
	setTraffic := provided["bandwidth"]
	trafficGB := u.TrafficQuotaGB
	if setTraffic {
		if trafficGB, err = mgr.ParseTrafficGB(trafficS); err != nil {
			return err
		}
	}

	if inter.IsTTY() {
		fmt.Printf("current quota: CPU %s / mem %d MiB / disk %d GiB / bandwidth %d GiB\n", mgr.FormatCPU(u.CPU), u.MemMB, u.DiskGB, u.TrafficQuotaGB)
		if !setCpu {
			s, err := inter.Ask("new CPU cores", mgr.FormatCPU(cpu), "", validateCPU)
			if err != nil {
				return err
			}
			cpu, _ = mgr.ParseCPU(s)
		}
		if !setMem {
			s, err := inter.Ask("new memory", strconv.Itoa(mem), " MiB (e.g. 512 or 1G)", validateMem)
			if err != nil {
				return err
			}
			mem, _ = parseMemStrict(s)
		}
		if !setDisk {
			s, err := inter.Ask("new disk", strconv.Itoa(disk), " GiB (only grow allowed)", validateDisk)
			if err != nil {
				return err
			}
			disk, _ = parseDiskStrict(s)
		}
		if !setTraffic {
			s, err := inter.Ask("new bandwidth quota", strconv.Itoa(trafficGB), " GiB/month (0 = unlimited)", validateTrafficGB)
			if err != nil {
				return err
			}
			trafficGB, _ = mgr.ParseTrafficGB(s)
		}
	}

	trafficChanged := setTraffic || trafficGB != u.TrafficQuotaGB
	if cpu == u.CPU && mem == u.MemMB && disk == u.DiskGB && !trafficChanged {
		if inter.IsTTY() {
			fmt.Println("no changes, exiting")
			return nil
		}
		return fmt.Errorf("nothing to update: pass at least one of --cpu/--mem/--disk/--bandwidth")
	}
	if disk < u.DiskGB {
		return fmt.Errorf("disk can only grow: current %d GiB, cannot shrink to %d GiB", u.DiskGB, disk)
	}

	m := mgr.New(c, d)
	if cpu != u.CPU || mem != u.MemMB || disk != u.DiskGB || trafficChanged {
		tgb := u.TrafficQuotaGB
		if trafficChanged {
			tgb = trafficGB
		}
		if _, err := m.UpdateQuotasAndTraffic(name, cpu, mem, disk, tgb); err != nil {
			return err
		}
	}
	fresh, err := d.GetUserByName(name)
	if err != nil {
		return err
	}
	printResult(m.ResultFor(fresh, ""))
	return nil
}

// userPasswd resets a user's panel login password to a random 20-char
// value and prints it once. The container root password is not touched.
func userPasswd(name string) error {
	if err := mgr.ValidateName(name); err != nil {
		return err
	}
	c, err := cfg.Load()
	if err != nil {
		return err
	}
	d, err := db.Open(c.Panel.DB)
	if err != nil {
		return err
	}
	defer d.Close()
	m := mgr.New(c, d)
	pass, err := m.ResetPanelPassword(name)
	if err != nil {
		return err
	}
	fmt.Printf("panel password reset for %s: %s\n", name, pass)
	return nil
}

func userPower(action, name string) error {
	c, err := cfg.Load()
	if err != nil {
		return err
	}
	d, err := db.Open(c.Panel.DB)
	if err != nil {
		return err
	}
	defer d.Close()
	m := mgr.New(c, d)
	if err := m.Power(name, action); err != nil {
		return err
	}
	fmt.Printf("user %s: %s ok\n", name, action)
	return nil
}

func userList() error {
	c, err := cfg.Load()
	if err != nil {
		return err
	}
	d, err := db.Open(c.Panel.DB)
	if err != nil {
		return err
	}
	defer d.Close()
	m := mgr.New(c, d)
	results, err := m.List()
	if err != nil {
		return err
	}
	fmt.Printf("%-16s %-14s %-14s %-10s %-6s %-8s %-7s %-8s %-8s %-6s %-10s\n", "NAME", "IP", "PORTS", "STATE", "CPU", "MEM", "DISK", "UP_GB", "DOWN_GB", "CPU%", "MEMUSE")
	for _, r := range results {
		ports := mgr.UserPorts(r.User.StartPort, r.PortsPerUser)
		if !r.V4Forward {
			ports = "v4-off"
		}
		fmt.Printf("%-16s %-14s %-14s %-10s %-6s %-8d %-7d %-8s %-8s %-6s %-10s\n",
			r.User.Name, r.User.IP, ports,
			r.State, mgr.FormatCPU(r.User.CPU), r.User.MemMB, r.User.DiskGB, r.UpGB, r.DownGB, r.CPUUse, r.MemUse)
	}
	return nil
}

func userShow(name string) error {
	c, err := cfg.Load()
	if err != nil {
		return err
	}
	d, err := db.Open(c.Panel.DB)
	if err != nil {
		return err
	}
	defer d.Close()
	m := mgr.New(c, d)
	res, err := m.Show(name)
	if err != nil {
		return err
	}
	printResult(res)
	return nil
}

// printAdded prints the essentials of a freshly created user. Live stats (CPU%,
// memory, traffic, domains) are all empty on a brand-new container and only add
// noise, so they are intentionally skipped here.
func printAdded(r *mgr.Result) {
	u := r.User
	fmt.Printf("name:     %s\n", u.Name)
	fmt.Printf("state:    %s\n", r.State)
	if r.V4Forward {
		fmt.Printf("ssh:      %d\n", u.SSHPort)
		fmt.Printf("ports:    %s\n", mgr.UserPorts(u.StartPort, r.PortsPerUser))
	} else {
		fmt.Printf("ssh:      %d (v4 inbound disabled — connect over IPv6)\n", u.SSHPort)
		fmt.Printf("ports:    %s (v4 inbound disabled)\n", mgr.UserPorts(u.StartPort, r.PortsPerUser))
	}
	fmt.Printf("quotas:   %s cpu / %d MiB / %d GiB / bandwidth %d GiB\n", mgr.FormatCPU(u.CPU), u.MemMB, u.DiskGB, u.TrafficQuotaGB)
	if r.IPv6 != "" {
		fmt.Printf("ipv6:     %s\n", r.IPv6)
	}
	if r.Password != "" {
		fmt.Printf("password: %s  (panel + root)\n", r.Password)
		if r.V4Forward {
			fmt.Printf("ssh:      ssh -p %d root@%s\n", u.SSHPort, r.PublicIP)
		} else {
			fmt.Printf("ssh:      v4 ssh unavailable (v6-only box) — ssh root@%s\n", r.IPv6)
		}
		c, _ := cfg.Load()
		fmt.Printf("panel:    %s\n", c.PanelURL(panelPath(c)))
	}
}

func printResult(r *mgr.Result) {
	u := r.User
	fmt.Printf("name:     %s\n", u.Name)
	fmt.Printf("state:    %s\n", r.State)
	fmt.Printf("ip:       %s\n", u.IP)
	if r.IPv6 != "" {
		fmt.Printf("ipv6:     %s\n", r.IPv6)
	}
	if r.V4Forward {
		fmt.Printf("ssh:      %d\n", u.SSHPort)
		fmt.Printf("ports:    %s\n", mgr.UserPorts(u.StartPort, r.PortsPerUser))
	} else {
		fmt.Printf("ssh:      %d (v4 inbound disabled — connect over IPv6)\n", u.SSHPort)
		fmt.Printf("ports:    %s (v4 inbound disabled)\n", mgr.UserPorts(u.StartPort, r.PortsPerUser))
	}
	fmt.Printf("quotas:   %s cpu / %d MiB / %d GiB / bandwidth %d GiB\n", mgr.FormatCPU(u.CPU), u.MemMB, u.DiskGB, u.TrafficQuotaGB)
	fmt.Printf("cpu use:  %s\n", r.CPUUse)
	fmt.Printf("mem use:  %s\n", r.MemUse)
	fmt.Printf("bandwidth:  up %s GB / down %s GB (this month)\n", r.UpGB, r.DownGB)
	fmt.Printf("domains:  %s\n", strings.Join(r.Domains, ", "))
	if r.Password != "" {
		c, _ := cfg.Load()
		fmt.Printf("password: %s  (panel + root)\n", r.Password)
		if r.V4Forward {
			fmt.Printf("ssh:      ssh -p %d root@%s\n", u.SSHPort, r.PublicIP)
		} else {
			fmt.Printf("ssh:      v4 ssh unavailable (v6-only box) — ssh root@%s\n", r.IPv6)
		}
		fmt.Printf("panel:    %s\n", c.PanelURL(panelPath(c)))
	}
}

var reInt = regexp.MustCompile(`^\d+$`)

func validateCPU(s string) error {
	_, err := mgr.ParseCPU(s)
	return err
}

// parseMemStrict parses a memory size into MiB: bare integer (MiB) or integer
// with M/G suffix. Decimals are rejected.
func parseMemStrict(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty input")
	}
	mult := 1
	last := s[len(s)-1]
	switch {
	case last >= '0' && last <= '9':
	case last == 'M' || last == 'm':
		s = s[:len(s)-1]
	case last == 'G' || last == 'g':
		mult = 1024
		s = s[:len(s)-1]
	default:
		return 0, fmt.Errorf("memory must be an integer in MiB (e.g. 512) or with a suffix (e.g. 1G)")
	}
	if !reInt.MatchString(s) {
		return 0, fmt.Errorf("memory must be an integer number of MiB (e.g. 512) or a suffix (e.g. 1G), not a decimal")
	}
	n, _ := strconv.Atoi(s)
	n *= mult
	if n < 64 {
		return 0, fmt.Errorf("memory must be at least 64 MiB")
	}
	return n, nil
}

func validateMem(s string) error {
	_, err := parseMemStrict(s)
	return err
}

// parseDiskStrict parses a disk size into GiB: bare integer or G suffix.
func parseDiskStrict(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty input")
	}
	if last := s[len(s)-1]; last == 'G' || last == 'g' {
		s = s[:len(s)-1]
	}
	if !reInt.MatchString(s) {
		return 0, fmt.Errorf("disk must be an integer number of GiB (e.g. 10 or 10G), not a decimal")
	}
	n, _ := strconv.Atoi(s)
	if n < 1 {
		return 0, fmt.Errorf("disk must be at least 1 GiB")
	}
	return n, nil
}

func validateDisk(s string) error {
	_, err := parseDiskStrict(s)
	return err
}

func validateTrafficGB(s string) error {
	_, err := mgr.ParseTrafficGB(s)
	return err
}
