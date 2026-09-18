package mgr

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
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
// Only the disk travels. Domains, SSH keys, quotas, expiry, bandwidth and
// resource history stay behind on the source: they are per-host configuration
// owned by that host's panel DB, and the two hosts deliberately keep separate
// sets (their admin keys are not the same). After the import the receiving host
// re-homes the container: the archive carries the SOURCE host's instance
// configuration, so this host's own spec — its IP, its ports, its quota — is
// written over it before the container is ever started.

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
}

// TransferManifest describes the artefact an export produced, which is what the
// receiving side needs in order to verify the download.
type TransferManifest struct {
	User    string
	Bytes   int64
	SHA256  string
	Elapsed time.Duration
}

// TransferResult describes a completed import, so the CLI can print the
// container's new connection details (which are this host's, not the source's).
type TransferResult struct {
	User      string
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
		Name:         "vpsmgr-transfer",
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
	}, nil
}

// TransferTarget resolves a receive target on this host, so the operator learns
// about a typo or a missing account before waiting out a long download rather
// than after it. The import re-checks all of it: it must not depend on having
// been called through here.
func (m *Manager) TransferTarget(name string) (*db.User, error) {
	if err := ValidateExistingName(name); err != nil {
		return nil, err
	}
	if g := ParseUserGroup(name); g.Child {
		return nil, fmt.Errorf("%s is part of user group %q — transfer the group's containers one at a time", name, g.Parent)
	}
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return nil, fmt.Errorf("no such user on this host (%v); create it first with: vps add %s", err, name)
	}
	return u, nil
}

// TransferImport replaces the user's container with the contents of a backup
// tarball read from r, re-homed onto this host's configuration, and returns the
// new connection details.
//
// The old container is never destroyed before the new one exists: it is renamed
// aside and deleted only once the replacement has been provisioned and started,
// so a failure at any point can put it back. The container keeps running when
// the command returns — this is the live copy now.
func (m *Manager) TransferImport(ctx context.Context, name string, r io.Reader, opt TransferOptions) (*TransferResult, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	u, err := m.TransferTarget(name)
	if err != nil {
		return nil, err
	}

	// Whether the container being replaced is running: if it is, put it back up
	// when anything fails.
	wasRunning := false
	if st, err := m.lx.State(u.Name); err == nil {
		wasRunning = st == "Running"
	}

	if err := m.db.UpdateUserStatus(u.ID, db.StatusReinstalling); err != nil {
		return nil, fmt.Errorf("mark reinstalling: %w", err)
	}

	started := time.Now()
	counting := &countingReader{r: r}
	tmp, err := tempCloneName(u.Name)
	if err != nil {
		return m.importFailed(u, wasRunning, err)
	}
	if err := m.lx.BackupImport(ctx, tmp, m.cfg.Incus.Pool, counting); err != nil {
		return m.importFailed(u, wasRunning, fmt.Errorf("import backup: %w", err))
	}
	// The archive carries the source host's configuration; replace it with ours
	// before the container can ever run here (own IP, own quota, own devices).
	spec, devices := m.instanceConfig(u)
	if err := m.lx.ReplaceConfig(tmp, spec, devices); err != nil {
		_ = m.lx.Delete(tmp)
		return m.importFailed(u, wasRunning, fmt.Errorf("apply local configuration: %w", err))
	}
	if err := m.ensureZfsRollbackVolume(tmp); err != nil {
		_ = m.lx.Delete(tmp)
		return m.importFailed(u, wasRunning, fmt.Errorf("zfs snapshot rollback setup: %w", err))
	}

	// Swap: park the old container under a temporary name (its disk survives),
	// then bring the imported one into place.
	if wasRunning {
		if err := m.lx.Stop(u.Name); err != nil {
			_ = m.lx.Delete(tmp)
			return m.importFailed(u, wasRunning, fmt.Errorf("stop old container: %w", err))
		}
	}
	aside, err := asideName(u.Name)
	if err != nil {
		_ = m.lx.Delete(tmp)
		return m.importFailed(u, wasRunning, err)
	}
	if err := m.lx.RenameInstance(u.Name, aside); err != nil {
		_ = m.lx.Delete(tmp)
		return m.importFailed(u, wasRunning, fmt.Errorf("move old container aside: %w", err))
	}
	if err := m.lx.RenameInstance(tmp, u.Name); err != nil {
		_ = m.lx.RenameInstance(aside, u.Name)
		_ = m.lx.Delete(tmp)
		return m.importFailed(u, wasRunning, fmt.Errorf("rename imported container into place: %w", err))
	}
	// The replaced container takes its snapshots (and any share over them) with
	// it when it is deleted below.
	_ = m.db.DeleteSnapshotShare(u.ID)

	pass, err := m.finishImport(u, aside, wasRunning)
	if err != nil {
		return nil, err
	}
	if err := m.lx.Delete(aside); err != nil {
		fmt.Printf("  ! warn: could not remove the replaced container %s: %v\n", aside, err)
	}
	if err := m.db.UpdateUserStatus(u.ID, db.StatusReady); err != nil {
		m.db.UpdateUserStatus(u.ID, db.StatusFailed)
		return nil, fmt.Errorf("db: mark user ready: %w", err)
	}
	m.limitMu.Lock()
	delete(m.throttled, u.Name)
	m.limitMu.Unlock()
	_ = m.db.AddAuditLog(opt.Actor, "transfer.import."+u.Name)

	return &TransferResult{
		User:      u.Name,
		RootPass:  pass,
		IP:        u.IP,
		SSHPort:   u.SSHPort,
		Ports:     UserPorts(u.StartPort, cfg.PortsPerUser),
		Elapsed:   time.Since(started),
		BytesRead: counting.n,
	}, nil
}

// finishImport starts the freshly imported container and rebuilds everything
// that must not be inherited from the source host: a fresh machine-id and SSH
// host keys (otherwise both hosts would present the same identity), the target
// user's own authorized_keys (the archive carries the source's), the root
// password, the hostname and the IPv6 wiring.
//
// Every failure rolls the swap back: the replacement is deleted and the old
// container returns to its name, so the user is never left with nothing.
func (m *Manager) finishImport(u *db.User, aside string, wasRunning bool) (string, error) {
	fail := func(err error) (string, error) {
		_ = m.UnwireIPv6(u.Name)
		_ = m.lx.Delete(u.Name)
		if rerr := m.lx.RenameInstance(aside, u.Name); rerr != nil {
			m.db.UpdateUserStatus(u.ID, db.StatusFailed)
			return "", fmt.Errorf("%w (and the previous container could not be restored: %v — it is parked as %s)", err, rerr, aside)
		}
		if wasRunning {
			_ = m.lx.Start(u.Name)
		}
		m.db.UpdateUserStatus(u.ID, db.StatusFailed)
		return "", err
	}

	if err := m.lx.Start(u.Name); err != nil {
		return fail(fmt.Errorf("start imported container: %w", err))
	}
	if err := m.lx.WaitReady(u.Name, 180*time.Second); err != nil {
		return fail(fmt.Errorf("wait for container: %w", err))
	}
	pass := pw.Generate(20)
	// image only picks the provisioning path; the rootfs came from another
	// vpsmgr host, so it is always one of the managed images.
	if err := m.Provision(u.Name, m.cfg.Incus.Image, pass); err != nil {
		return fail(fmt.Errorf("provision container: %w", err))
	}
	// The container brings the source's machine-id (a duplicate DUID makes
	// dnsmasq drop DHCPv6 leases) and the source's SSH host keys (which would
	// make two hosts claim the same identity to every client).
	if err := m.regenerateMachineID(u.Name); err != nil {
		return fail(fmt.Errorf("regenerate machine-id: %w", err))
	}
	if err := m.regenerateSSHHostKeys(u.Name); err != nil {
		return fail(fmt.Errorf("regenerate ssh host keys: %w", err))
	}
	// ...and the source's authorized_keys: clear them before writing this
	// host's own, or the source could still log in.
	m.clearAuthorizedKeys(u.Name)
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
	m.applyUserKeys(u.Name)
	return pass, nil
}

// importFailed records the failure and returns the error unchanged. The old
// container is still under its own name whenever this is called (the swap
// either never started or was already rolled back), so there is nothing to
// restore here — only the running state we took away.
func (m *Manager) importFailed(u *db.User, wasRunning bool, err error) (*TransferResult, error) {
	m.db.UpdateUserStatus(u.ID, db.StatusFailed)
	if wasRunning {
		_ = m.lx.Start(u.Name)
	}
	return nil, err
}

// instanceConfig is the container specification this host would give the user
// today: its own static IPv4, IPv6 assignment, NIC layout and quota. Writing it
// over an imported container is what stops the source's network configuration
// from following the disk here.
func (m *Manager) instanceConfig(u *db.User) (map[string]string, map[string]lx.Device) {
	poolMode := m.cfg.IPv6ModeEffective() == cfg.IPv6ModePool
	ipv6, block := "", ""
	if !poolMode {
		ipv6, _ = m.IPv6Addr(u.Name)
		if b, _ := m.IPv6Block(u.Name); b != nil {
			block = b.String()
		}
	}
	poolAddr := ""
	if poolMode {
		// Only pool mode keeps the address on the routed NIC, and only when
		// the user actually has one.
		poolAddr = u.IPv6Address
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

// asideName is the transient name the replaced container is parked under during
// the swap. Like tempCloneName it never ends in a bare number, so it is never
// mistaken for a user group's child account.
func asideName(name string) (string, error) {
	b := make([]byte, 2)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-old-%04x", name, b), nil
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
