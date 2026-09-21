package mgr

import (
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"time"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
	"vpsmgr/internal/lx"
	"vpsmgr/internal/pw"
)

// snapshot sharing
//
// A user may publish one checkpoint behind a random code. Anyone can install a
// container by presenting that code: the snapshot is cloned copy-on-write
// (no image is built, so it costs no real disk space until it diverges) and the
// clone is re-provisioned as the target user's own container — own network,
// quota, hostname, root password and SSH keys. The source container and its
// snapshot are untouched, so one share can serve many installs.
//
// Because a clone pins its origin snapshot, a checkpoint that has been cloned
// can no longer be rolled back past. Publishing therefore deletes the OLDER
// checkpoints (they could never be restored to again anyway) and the panel
// warns about it first.

// shareCodeLen is the number of random UUID bits presented to users. 128 bits
// (a v4 UUID) is unguessable.
func newShareCode() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// olderSnapshotNames returns the names of snapshots created strictly before
// target, preserving list order. A snapshot with an unparseable timestamp is
// left alone (safe: it is never deleted).
func olderSnapshotNames(snaps []lx.SnapshotInfo, target string) []string {
	var targetTime time.Time
	found := false
	for _, s := range snaps {
		if s.Name == target {
			targetTime = snapshotCreateTime(s)
			found = !targetTime.IsZero()
			break
		}
	}
	if !found {
		return nil
	}
	var older []string
	for _, s := range snaps {
		if s.Name == target {
			continue
		}
		if t := snapshotCreateTime(s); !t.IsZero() && t.Before(targetTime) {
			older = append(older, s.Name)
		}
	}
	return older
}

// SnapshotShareEnabled reports whether snapshot sharing is enabled: the DB
// mirror written by `vps config set snapshots.share`, falling back to the
// config file when unset. The mirror is what lets the running panel see the
// change without a restart (same pattern as net.v4_forward).
func (m *Manager) SnapshotShareEnabled() bool {
	if v, ok, err := m.db.SnapshotShareEnabled(); err == nil && ok {
		return v == "1"
	}
	return m.cfg.Snapshots.Share
}

// SetSnapshotShareEnabled writes the mirror so the running panel applies the
// toggle immediately. Called by `vps config set snapshots.share` and by
// `vps install`. Disabling keeps the stored codes and leaves existing
// containers and checkpoints untouched.
func (m *Manager) SetSnapshotShareEnabled(on bool) error {
	return m.db.SetSnapshotShareEnabled(on)
}

// ShareSnapshot publishes a checkpoint: it deletes the older checkpoints (which
// become unreachable once the clone exists) and stores a fresh code, replacing
// any previous share. It returns the code and the names of the removed
// checkpoints so the caller can tell the user what happened.
func (m *Manager) ShareSnapshot(name, snapName string) (string, []string, error) {
	if !m.SnapshotShareEnabled() {
		return "", nil, errors.New("snapshot sharing is disabled by the administrator")
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()

	u, err := m.db.GetUserByName(name)
	if err != nil {
		return "", nil, err
	}
	if m.SnapshotLimit() == 0 {
		return "", nil, errors.New("snapshots are disabled")
	}
	if !ValidSnapName(snapName) {
		return "", nil, errors.New("invalid snapshot name")
	}
	snaps, err := m.lx.SnapshotList(u.Name)
	if err != nil {
		return "", nil, err
	}
	found := false
	for _, s := range snaps {
		if s.Name == snapName {
			found = true
			break
		}
	}
	if !found {
		return "", nil, errors.New("snapshot not found")
	}
	removed := olderSnapshotNames(snaps, snapName)
	for _, s := range removed {
		if err := m.lx.SnapshotDelete(u.Name, s); err != nil {
			return "", nil, fmt.Errorf("delete older checkpoint %s: %w", s, err)
		}
		_ = m.db.DeleteSnapshotShareBySnapshot(u.ID, s)
	}
	code, err := newShareCode()
	if err != nil {
		return "", nil, err
	}
	if err := m.db.SetSnapshotShare(u.ID, code, snapName); err != nil {
		return "", nil, err
	}
	return code, removed, nil
}

// SnapshotShareInfo returns the user's current share code and the checkpoint it
// points at (ok=false when the user has no share).
func (m *Manager) SnapshotShareInfo(name string) (code, snap string, ok bool) {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return "", "", false
	}
	s, err := m.db.GetSnapshotShare(u.ID)
	if err != nil || s == nil {
		return "", "", false
	}
	return s.Code, s.Snapshot, true
}

// RevokeSnapshotShare drops the user's share code.
func (m *Manager) RevokeSnapshotShare(name string) error {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return err
	}
	return m.db.DeleteSnapshotShare(u.ID)
}

// resolveShare turns a code into its owner and snapshot, verifying the
// checkpoint still exists (the owner may have deleted it, reinstalled or
// removed the container since publishing).
func (m *Manager) resolveShare(code string) (*db.User, string, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return nil, "", errors.New("no share code given")
	}
	share, err := m.db.GetSnapshotShareByCode(code)
	if err != nil {
		return nil, "", err
	}
	if share == nil {
		return nil, "", errors.New("invalid or expired share code")
	}
	owner, err := m.db.GetUserByID(share.UserID)
	if err != nil {
		return nil, "", errors.New("invalid or expired share code")
	}
	snaps, err := m.lx.SnapshotList(owner.Name)
	if err != nil {
		return nil, "", err
	}
	for _, s := range snaps {
		if s.Name == share.Snapshot {
			return owner, share.Snapshot, nil
		}
	}
	return nil, "", errors.New("the shared checkpoint no longer exists (the owner deleted it)")
}

// ResolveShare returns the owner container and checkpoint behind a share code,
// so callers can record where an install came from (audit) before acting. It
// validates exactly like an install does.
func (m *Manager) ResolveShare(code string) (owner, snap string, err error) {
	u, snap, err := m.resolveShare(code)
	if err != nil {
		return "", "", err
	}
	return u.Name, snap, nil
}

// ReinstallFromShare rebuilds the user's container from a shared checkpoint.
// When the code points at a checkpoint of the very container being reinstalled,
// the operation is a plain rollback (the snapshot would be destroyed if the
// container were deleted first). Otherwise the snapshot is cloned to a
// temporary container, the old container is deleted, and the clone is renamed
// into place — so a clone failure never leaves the user with nothing.
//
// runInit is the opt-in "run my init script" switch from the reinstall form. It
// applies only to the cross-container clone: a clone carries the source
// checkpoint's state, so its init script is skipped unless the user asked for
// it. Rolling back the user's own checkpoint ignores it entirely.
func (m *Manager) ReinstallFromShare(name, code string, runInit bool) (string, error) {
	if !m.SnapshotShareEnabled() {
		return "", errors.New("snapshot sharing is disabled by the administrator")
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()

	u, err := m.db.GetUserByName(name)
	if err != nil {
		return "", err
	}
	owner, snap, err := m.resolveShare(code)
	if err != nil {
		return "", err
	}

	// Own checkpoint: a time-machine restore of the same container is exactly
	// "reinstall from this checkpoint", and avoids deleting the source of the
	// clone. Disk-only keeps the current network/quotas.
	if owner.ID == u.ID {
		if err := m.snapshotRestoreLocked(u, snap); err != nil {
			return "", err
		}
		m.applyUserKeys(u.Name)
		// runInit is ignored here: the state came back from the checkpoint,
		// so there is no image rebuild for the script to follow.
		return "", nil
	}

	// Cross-container clone.
	if err := m.db.UpdateUserStatus(u.ID, db.StatusReinstalling); err != nil {
		return "", fmt.Errorf("mark reinstalling: %w", err)
	}
	tmp, err := tempCloneName(u.Name)
	if err != nil {
		m.db.UpdateUserStatus(u.ID, db.StatusFailed)
		return "", err
	}
	blockStr := ""
	if m.cfg.IPv6ModeEffective() != cfg.IPv6ModePool {
		if block, _ := m.IPv6Block(u.Name); block != nil {
			blockStr = block.String()
		}
		// The account keeps its whole /64 across the reinstall, so the clone
		// gets the same declared routes.
		blockStr = blockRoutes(blockStr, u.IPv6ExtraBlock)
	}
	if err := m.lx.CloneFromSnapshot(owner.Name, snap, tmp,
		m.cfg.Incus.Pool, m.cfg.Incus.Bridge, u.IP, "", blockStr, u.IPv6Address,
		m.cfg.Net.ExtIF, u.CPU, u.MemMB, u.DiskGB); err != nil {
		m.db.UpdateUserStatus(u.ID, db.StatusFailed)
		return "", fmt.Errorf("clone shared checkpoint: %w", err)
	}
	rollback := func() error {
		_ = m.UnwireIPv6(tmp)
		_ = m.lx.Delete(tmp)
		_ = m.lx.Delete(u.Name)
		_ = m.db.UpdateUserStatus(u.ID, db.StatusFailed)
		return nil
	}
	if err := m.lx.Delete(u.Name); err != nil {
		rollback()
		return "", fmt.Errorf("delete old container: %w", err)
	}
	// The old container's snapshots (and any share over them) are gone with it.
	_ = m.db.DeleteSnapshotShare(u.ID)
	if err := m.lx.RenameInstance(tmp, u.Name); err != nil {
		rollback()
		return "", fmt.Errorf("rename clone into place: %w", err)
	}
	if err := m.ensureZfsRollbackVolume(u.Name); err != nil {
		rollback()
		return "", fmt.Errorf("zfs snapshot rollback setup: %w", err)
	}
	if err := m.lx.Start(u.Name); err != nil {
		rollback()
		return "", fmt.Errorf("start container: %w", err)
	}
	if err := m.lx.WaitReady(u.Name, 120*time.Second); err != nil {
		rollback()
		return "", fmt.Errorf("wait for container: %w", err)
	}
	pass := pw.Generate(20)
	// image is only used to pick the provisioning path; a clone always comes
	// from a vpsmgr image, so pass the managed alias.
	if err := m.Provision(u.Name, m.cfg.Incus.Image, pass); err != nil {
		rollback()
		return "", fmt.Errorf("provision container: %w", err)
	}
	// The clone carries the source's machine-id; a duplicate DUID makes dnsmasq
	// drop DHCPv6 leases, so give it a fresh one before the network settles.
	if err := m.regenerateMachineID(u.Name); err != nil {
		rollback()
		return "", fmt.Errorf("regenerate machine-id: %w", err)
	}
	// The clone also carries the source's authorized_keys — clear them before
	// writing this user's own keys, or the source could log in.
	m.clearAuthorizedKeys(u.Name)
	if m.cfg.IPv6ModeEffective() == cfg.IPv6ModePool {
		if u.IPv6Address != "" {
			if err := m.ConfigureContainerIPv6(u.Name, u.IPv6Address, ""); err != nil {
				rollback()
				return "", fmt.Errorf("config container ipv6: %w", err)
			}
			if err := m.WireIPv6Pool(u.Name, u.IPv6Address); err != nil {
				rollback()
				return "", fmt.Errorf("wire ipv6 pool: %w", err)
			}
		}
	} else {
		if err := m.ConfigureContainerIPv6(u.Name, "", ""); err != nil {
			rollback()
			return "", fmt.Errorf("config container ipv6: %w", err)
		}
		if err := m.WireIPv6(u.Name, nil, nil); err != nil {
			rollback()
			return "", fmt.Errorf("wire ipv6: %w", err)
		}
	}
	// The clone already carries the checkpoint's state, so the init script is
	// opt-in here: it runs only if the user ticked the box on the reinstall
	// form. Best-effort, exactly like a plain reinstall — a delivery failure
	// never fails the rebuild, the container is already usable.
	if runInit && u.InitScript != "" {
		if err := m.lx.RunInitScript(u.Name, u.InitScript); err != nil {
			fmt.Printf("  ! warn: init script: %v (container still recreated)\n", err)
		}
	}
	m.applyUserKeys(u.Name)
	if err := m.db.UpdateUserStatus(u.ID, db.StatusReady); err != nil {
		m.db.UpdateUserStatus(u.ID, db.StatusFailed)
		return "", fmt.Errorf("db: mark user ready: %w", err)
	}
	m.limitMu.Lock()
	delete(m.throttled, u.Name)
	m.limitMu.Unlock()
	return pass, nil
}

// tempCloneName is a transient container name for the clone-in-place dance. It
// carries no trailing digits after a hyphen, so it is never parsed as a child
// account.
func tempCloneName(name string) (string, error) {
	b := make([]byte, 2)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-rt-%04x", name, b), nil
}

// regenerateMachineID gives a cloned container a fresh machine-id.
func (m *Manager) regenerateMachineID(name string) error {
	_, err := m.lx.ExecSH(name, "rm -f /etc/machine-id /var/lib/dbus/machine-id && systemd-machine-id-setup >/dev/null 2>&1")
	return err
}

// clearAuthorizedKeys drops the container's authorized_keys (a clone inherits
// the source's). The next ApplySSHKeys rebuilds it from the DB.
func (m *Manager) clearAuthorizedKeys(name string) {
	_, _ = m.lx.ExecSH(name, "rm -f /root/.ssh/authorized_keys")
}

// applyUserKeys best-effort re-applies the user's active (and granted admin)
// keys. Keys persist in the DB, so a failure is not fatal.
func (m *Manager) applyUserKeys(name string) {
	active, err := m.ActiveKeys(name)
	if err != nil {
		return
	}
	admin, _ := m.GrantedAdminKeys(name)
	if len(active) == 0 && len(admin) == 0 {
		return
	}
	if err := m.ApplySSHKeys(name, active, admin); err != nil {
		fmt.Printf("  ! warn: ssh keys: %v\n", err)
	}
}
