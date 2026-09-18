package mgr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
	"vpsmgr/internal/lx"
	"vpsmgr/internal/pw"
)

// cross-machine transfer
//
// `vps transfer` moves one container's root disk between two vpsmgr hosts that
// can reach each other over HTTPS but share nothing else. The source host
// exports the stopped container to a single backup tarball, serves it from a
// temporary listener, and the receiving host imports it over that listener.
//
// Receiving is creating an account: the target name must be free, and the
// import builds it the way `vps add` would — same allocation code, same
// firewall rules, same panel row — then replaces the disk of the container it
// just created with the transferred one. Nothing that already exists on the
// receiving host is touched, so a failure anywhere is undone by deleting what
// was created, and the machine is exactly as it was.
//
// Domains, SSH keys, sticky notes and bandwidth history stay behind on the
// source: they are per-host configuration owned by that host's panel DB, and
// the two hosts deliberately keep separate sets (their admin keys are not the
// same). The account's quota does travel — the receive command carries it.

// TransferOptions carries the operator's choices for one transfer.
type TransferOptions struct {
	// Optimized asks Incus for a storage-driver native stream instead of a
	// portable tarball. Much faster, but the file can only be restored into a
	// pool running the same driver, so it is opt-in and the receiving side
	// fails loudly if the driver does not match.
	Optimized bool
	// Compression is "none", "gzip", "zstd" or "" for the Incus default.
	Compression string
	// Actor is recorded in the audit log (the operator running the command).
	Actor string
	// Account is how the receiving host sizes the account it creates.
	Account AddOptions
}

// TransferManifest describes the artefact an export produced, and the account
// settings the receiving host should recreate, so the printed receive command
// carries both.
type TransferManifest struct {
	User    string
	Bytes   int64
	SHA256  string
	Elapsed time.Duration
	Account AddOptions
}

// TransferResult describes a completed import, so the CLI can print the
// container's connection details (which are this host's, not the source's).
type TransferResult struct {
	User      string
	PanelPass string
	RootPass  string
	IP        string
	SSHPort   int
	Ports     string
	Elapsed   time.Duration
	BytesRead int64
}

// TransferEstimate returns the number of bytes the container's root disk
// currently occupies, used to check the temp file will fit before a long export
// is started. It falls back to the user's disk quota when the daemon has no
// exact figure (a stopped container on some storage drivers reports none).
func (m *Manager) TransferEstimate(name string) (int64, error) {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return 0, err
	}
	if n, err := m.lx.DiskUsage(u.Name); err == nil && n > 0 {
		return n, nil
	}
	if all, err := m.lx.Metrics(); err == nil {
		if v, ok := all[u.Name]; ok && v.FilesystemSize > 0 && v.FilesystemAvail <= v.FilesystemSize {
			return v.FilesystemSize - v.FilesystemAvail, nil
		}
	}
	return int64(u.DiskGB) << 30, nil
}

// TransferExport streams the user's stopped container to w and returns the
// manifest of what was written.
//
// The container must already be stopped by the operator: exporting a running
// container copies a live filesystem, and this command never changes a
// container's power state — a transfer must not be the thing that decides a
// user's downtime.
func (m *Manager) TransferExport(ctx context.Context, name string, w io.Writer, opt TransferOptions) (*TransferManifest, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	u, err := m.db.GetUserByName(name)
	if err != nil {
		return nil, err
	}
	if err := m.requireStopped(u.Name); err != nil {
		return nil, err
	}

	started := time.Now()
	sum := sha256.New()
	n, err := m.lx.BackupExport(ctx, u.Name, lx.BackupOptions{
		Compression:  opt.Compression,
		Optimized:    opt.Optimized,
		InstanceOnly: true, // the container as it is now, not its snapshot history
		RootOnly:     true, // no dependent volumes
	}, io.MultiWriter(w, sum))
	if err != nil {
		return nil, err
	}
	_ = m.db.AddAuditLog(opt.Actor, "transfer.export."+u.Name)
	return &TransferManifest{
		User:    u.Name,
		Bytes:   n,
		SHA256:  hex.EncodeToString(sum.Sum(nil)),
		Elapsed: time.Since(started),
		Account: accountFor(u),
	}, nil
}

// accountFor is the account a transfer recreates on the far side: the quota the
// container has here, expressed as the flags `vps add` takes.
func accountFor(u *db.User) AddOptions {
	opt := AddOptions{
		CPU:         u.CPU,
		MemMB:       u.MemMB,
		DiskGB:      u.DiskGB,
		BandwidthGB: u.BandwidthQuotaGB,
	}
	if u.ExpiresAt != "" {
		if left := daysUntil(u.ExpiresAt); left > 0 {
			opt.Days = left
		}
	}
	return opt
}

// daysUntil rounds an expiry deadline up to whole days, so a transfer never
// shortens the account it recreates.
func daysUntil(rfc3339 string) int {
	t, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return 0
	}
	d := time.Until(t)
	if d <= 0 {
		return 0
	}
	return int((d + 24*time.Hour - 1) / (24 * time.Hour))
}

// TransferTarget checks the receiving name is usable, so the operator learns
// about a typo — or about a name that is already taken — before waiting out a
// long download rather than after it. The import re-checks it: it must not
// depend on having been called through here.
func (m *Manager) TransferTarget(name string) error {
	name = strings.ToLower(name)
	if err := ValidateName(name); err != nil {
		return err
	}
	if g := ParseUserGroup(name); g.Child {
		return fmt.Errorf("%s is part of a user group — transfer the group's containers one at a time", name)
	}
	if _, err := m.db.GetUserByName(name); err == nil {
		return fmt.Errorf("%s already exists on this host, and a transfer creates a new account; delete it first with: vps del %s", name, name)
	}
	return nil
}

// TransferImport creates the account from a backup tarball read from r and
// returns its connection details.
//
// The account is created first, exactly as `vps add` would create it — so its
// IP, port block, IPv6 assignment, firewall rules and panel row all come from
// the same code path as any other container — and the disk of the container
// that produced is then replaced by the transferred one. Nothing that existed
// before this call is modified, so any failure is undone by deleting what was
// created.
func (m *Manager) TransferImport(ctx context.Context, name string, r io.Reader, opt TransferOptions) (*TransferResult, error) {
	// No opMu here: Add and Del take it themselves, and this runs the two of
	// them in sequence.
	if err := m.TransferTarget(name); err != nil {
		return nil, err
	}
	started := time.Now()
	counting := &countingReader{r: r}

	created, err := m.Add(name, opt.Account)
	if err != nil {
		return nil, err
	}
	u := created.User

	// From here the whole account — container, firewall rules, DB row — exists
	// and belongs to this import, so every failure path below removes it and
	// leaves the host as it was.
	fail := func(err error) (*TransferResult, error) {
		if derr := m.Del(u.Name); derr != nil {
			fmt.Printf("  ! warn: could not remove the half-built account %s: %v\n", u.Name, derr)
		}
		return nil, err
	}

	// The container Add just made has to go before the transfer's disk is
	// imported in its place. Deleting it is not merely tidiness: Incus serves a
	// container its static IPv4 from a DHCP reservation that the current lease
	// holds until the instance is gone, so a replacement that appears while the
	// first container still exists is handed a dynamic address from the pool —
	// and then nothing can reach it, because the whole hosting setup (panel
	// records, DNAT rules, what the user was told) names the static one.
	if err := m.lx.Stop(u.Name); err != nil {
		return fail(fmt.Errorf("stop the new container: %w", err))
	}
	if err := m.lx.Delete(u.Name); err != nil {
		return fail(fmt.Errorf("remove the new container: %w", err))
	}

	// The archive carries the source host's devices, which Incus validates
	// against this host's networks as it creates the instance, so this host's
	// own devices have to go in as part of the import rather than after it.
	spec, devices := m.instanceConfig(u)
	if err := m.lx.BackupImport(ctx, u.Name, m.cfg.Incus.Pool, devices, counting); err != nil {
		return fail(fmt.Errorf("import backup: %w", err))
	}
	// ...and the configuration is then replaced outright, which also clears
	// whatever device keys the source had that this host's spec does not set.
	if err := m.lx.ReplaceConfig(u.Name, spec, devices); err != nil {
		return fail(fmt.Errorf("apply local configuration: %w", err))
	}
	if err := m.ensureZfsRollbackVolume(u.Name); err != nil {
		return fail(fmt.Errorf("zfs snapshot rollback setup: %w", err))
	}
	if err := m.lx.Start(u.Name); err != nil {
		return fail(fmt.Errorf("start imported container: %w", err))
	}
	if err := m.lx.WaitReady(u.Name, 180*time.Second); err != nil {
		return fail(fmt.Errorf("wait for container: %w", err))
	}

	// The transferred disk still carries the source's root password, hostname,
	// machine-id and SSH host keys. Rebuilding all of them is what makes the
	// container this host's own rather than a copy of the other one: a shared
	// machine-id makes dnsmasq drop DHCPv6 leases, and shared host keys would
	// let two machines claim one identity to every client.
	pass := pw.Generate(20)
	// image only picks the provisioning path; the rootfs came from another
	// vpsmgr host, so it is always one of the managed images.
	if err := m.Provision(u.Name, m.cfg.Incus.Image, pass); err != nil {
		return fail(fmt.Errorf("provision container: %w", err))
	}
	if err := m.regenerateMachineID(u.Name); err != nil {
		return fail(fmt.Errorf("regenerate machine-id: %w", err))
	}
	if err := m.regenerateSSHHostKeys(u.Name); err != nil {
		return fail(fmt.Errorf("regenerate ssh host keys: %w", err))
	}
	// ...and the source's authorized_keys, or the source could still log in.
	m.clearAuthorizedKeys(u.Name)

	// The source's IPv6 stanza is still in the container's network config, and
	// this host's is appended rather than written, so it has to go first.
	if err := m.stripForeignIPv6(u.Name); err != nil {
		return fail(fmt.Errorf("clear the source host's ipv6 configuration: %w", err))
	}
	if m.cfg.IPv6ModeEffective() == cfg.IPv6ModePool {
		if u.IPv6Address != "" {
			if err := m.ConfigureContainerIPv6(u.Name, u.IPv6Address); err != nil {
				return fail(fmt.Errorf("config container ipv6: %w", err))
			}
			if err := m.WireIPv6Pool(u.Name, u.IPv6Address); err != nil {
				return fail(fmt.Errorf("wire ipv6 pool: %w", err))
			}
		}
	} else {
		if err := m.ConfigureContainerIPv6(u.Name, ""); err != nil {
			return fail(fmt.Errorf("config container ipv6: %w", err))
		}
		if err := m.WireIPv6(u.Name); err != nil {
			return fail(fmt.Errorf("wire ipv6: %w", err))
		}
	}
	if err := m.ensureStaticIPv4(u); err != nil {
		return fail(err)
	}
	m.applyUserKeys(u.Name)

	m.limitMu.Lock()
	delete(m.throttled, u.Name)
	m.limitMu.Unlock()
	_ = m.db.AddAuditLog(opt.Actor, "transfer.import."+u.Name)

	return &TransferResult{
		User:      u.Name,
		PanelPass: created.Password,
		RootPass:  pass,
		IP:        u.IP,
		SSHPort:   u.SSHPort,
		Ports:     UserPorts(u.StartPort, cfg.PortsPerUser),
		Elapsed:   time.Since(started),
		BytesRead: counting.n,
	}, nil
}

// stripForeignIPv6 removes the IPv6 configuration that the host this container
// came from left in its networkd file.
//
// The re-key step appends this host's address, routed block and gateway
// neighbour, and only checks for its OWN values before appending — which is
// right for a container that was always here, but a container that arrives
// from another host already carries that host's address and block. Left alone
// it comes up with two global addresses, one of them belonging to a network it
// is not on, and services can bind to the wrong one.
//
// Only stanzas whose values are not this host's are dropped; the image's own
// DHCP setup is kept. Pool mode rewrites the file outright, so it has nothing
// to strip.
func (m *Manager) stripForeignIPv6(name string) error {
	if !m.cfg.IPv6Enabled() || m.cfg.IPv6ModeEffective() == cfg.IPv6ModePool {
		return nil
	}
	ipv6, err := m.IPv6Addr(name)
	if err != nil || ipv6 == "" {
		return err
	}
	block := ""
	if b, _ := m.IPv6Block(name); b != nil {
		block = b.String()
	}
	script := `set -e
CFG=/etc/systemd/network/eth0.network
[ -f "$CFG" ] || exit 0
awk -v keep_addr=` + strconv.Quote(ipv6+"/128") + ` -v keep_block=` + strconv.Quote(block) + ` -v keep_mac=` + strconv.Quote(m.bridgeMAC()) + ` '
function reset() { hdr=""; body=""; addr=""; dest=""; lladdr=""; lcl=0 }
function keepit() {
  if (hdr == "[Address]" && addr != "" && addr != keep_addr) return 0
  if (hdr == "[Route]" && lcl == 1 && dest != "" && dest != keep_block) return 0
  if (hdr == "[Neighbor]" && lladdr != "" && lladdr != keep_mac) return 0
  return 1
}
function flush() { if (hdr != "" && keepit()) print body; reset() }
BEGIN { ORS="" }
/^\[/ { flush(); hdr=$0 }
{ body = body $0 "\n" }
/^Address=/ && hdr == "[Address]" { addr=substr($0,9) }
/^Destination=/ { dest=substr($0,13) }
/^Type=local/ { lcl=1 }
/^LinkLayerAddress=/ { lladdr=substr($0,18) }
END { flush() }
' "$CFG" > "$CFG.new"
mv "$CFG.new" "$CFG"
`
	_, err = m.lx.ExecSH(name, script)
	return err
}

// ensureStaticIPv4 checks that the container actually holds the IPv4 address its
// whole hosting setup is built around: the panel's records, the host's DNAT
// rules and the user's own notes all name it, so a container that ends up on
// another address is unreachable. Better to say so than to hand over a machine
// nobody can connect to.
func (m *Manager) ensureStaticIPv4(u *db.User) error {
	if u.IP == "" {
		return nil
	}
	out, err := m.lx.ExecSH(u.Name, "ip -4 -o addr show dev eth0 2>/dev/null | awk '{print $4}'")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), u.IP+"/") {
			return nil
		}
	}
	return fmt.Errorf("the container came up on %s, not on its assigned %s — its port forwarding will not work until it takes that address",
		strings.Join(strings.Fields(out), " "), u.IP)
}

// instanceConfig is the container specification this host gives the user: its
// own static IPv4, IPv6 assignment, NIC layout and quota. Writing it over an
// imported container is what stops the source's network configuration from
// following the disk here.
func (m *Manager) instanceConfig(u *db.User) (map[string]string, map[string]lx.Device) {
	poolMode := m.cfg.IPv6ModeEffective() == cfg.IPv6ModePool
	ipv6, block, poolAddr := "", "", ""
	if poolMode {
		// Only pool mode keeps the address on the routed NIC, and only when
		// the user actually has one.
		poolAddr = u.IPv6Address
	} else {
		ipv6, _ = m.IPv6Addr(u.Name)
		if b, _ := m.IPv6Block(u.Name); b != nil {
			block = b.String()
		}
	}
	return m.lx.InstanceSpec(m.cfg.Incus.Pool, m.cfg.Incus.Bridge, u.IP,
		ipv6, block, poolAddr, m.cfg.Net.ExtIF, u.CPU, u.MemMB, u.DiskGB)
}

// requireStopped refuses to export a container that is not stopped. The check
// reads the live status rather than trusting the caller: the operator may have
// started the container again since running the command.
func (m *Manager) requireStopped(name string) error {
	all, err := m.lx.InstanceStatuses()
	if err != nil {
		return err
	}
	st, ok := all[name]
	if !ok {
		return fmt.Errorf("container %s does not exist", name)
	}
	if st != "Stopped" {
		return fmt.Errorf("container %s is %s — stop it first: vps power %s stop", name, st, name)
	}
	return nil
}

// regenerateSSHHostKeys gives an imported container its own sshd host keys. A
// root filesystem copied from another host carries that host's keys, so without
// this both machines would present the same identity to every client.
func (m *Manager) regenerateSSHHostKeys(name string) error {
	_, err := m.lx.ExecSH(name, "rm -f /etc/ssh/ssh_host_* && ssh-keygen -A")
	return err
}

// countingReader counts the bytes a transfer actually read from the network, so
// the receiving side can report the size it consumed.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
