package mgr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
	"vpsmgr/internal/lx"
)

// cross-machine transfer
//
// `vps transfer` moves one container's root disk between two vpsmgr hosts that
// can reach each other over HTTPS but share nothing else. The source host
// exports the stopped container to a backup tarball and serves it, together
// with a small JSON description of the account, from a temporary listener; the
// receiving host imports the disk over that listener and recreates the account.
//
// The point is that the owner should notice nothing but their address: the
// quota, the SSH keys, the sticky notes and the init script all travel in that
// JSON, so none of it has to be retyped on a command line or remembered. What
// does NOT travel is everything that belongs to the source as a machine —
// domains, admin key grants, bandwidth history, snapshots (which stay on the
// host that took them) — and the receiving host's own IP, port block and IPv6
// assignment, which stay randomly allocated as always.

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

// TransferMeta is the account's state as it travels with the archive: what the
// receiving host needs in order to put the account back the way it was, and
// how much room the disk it is about to receive needs.
type TransferMeta struct {
	User        string    `json:"user"`
	CPU         int       `json:"cpu"`
	MemMB       int       `json:"mem_mb"`
	DiskGB      int       `json:"disk_gb"`
	BandwidthGB int       `json:"bandwidth_gb"`
	ExpiresAt   string    `json:"expires_at,omitempty"`
	DiskUsed    int64     `json:"disk_used"`
	InitScript  string    `json:"init_script,omitempty"`
	SSHKeys     []MetaKey `json:"ssh_keys,omitempty"`
	StickyNotes string    `json:"sticky_notes,omitempty"`
}

// MetaKey is one of the user's public keys, as the panel stores it.
type MetaKey struct {
	Name   string `json:"name"`
	Key    string `json:"key"`
	Active bool   `json:"active"`
}

// Account is the meta as the flags `vps add` takes. The deadline is not part of
// it: it is an absolute date, applied on its own after the account exists.
func (t *TransferMeta) Account() AddOptions {
	return AddOptions{
		CPU:         t.CPU,
		MemMB:       t.MemMB,
		DiskGB:      t.DiskGB,
		BandwidthGB: t.BandwidthGB,
	}
}

// TransferManifest describes the artefact an export produced, so the receiving
// side can verify both files it is served.
type TransferManifest struct {
	User     string
	Bytes    int64
	SHA256   string
	Meta     []byte
	MetaSHA  string
	DiskUsed int64
	Elapsed  time.Duration
}

// TransferResult describes a completed import, so the CLI can print the
// container's connection details (which are this host's, not the source's).
type TransferResult struct {
	User      string
	PanelPass string
	IP        string
	SSHPort   int
	Ports     string
	Elapsed   time.Duration
	BytesRead int64
}

// requireSupportedDriver refuses a transfer on a storage driver the feature
// does not cover.
//
// Only zfs is supported. The other two drivers in this project cannot carry a
// container between hosts the way this works: a btrfs pool would need both
// hosts to agree on a driver-specific stream, and a dir pool has no snapshots
// and no quotas behind it at all — it exists as a test-box opt-in. Refusing
// outright is the honest answer, and it removes any question of a driver
// mismatch being discovered halfway through a migration.
func (m *Manager) requireSupportedDriver() error {
	driver, err := m.lx.PoolDriver(m.cfg.Incus.Pool)
	if err != nil {
		return err
	}
	if driver != "zfs" {
		return fmt.Errorf("moving a container needs the zfs storage driver; this host's pool %q uses %q",
			m.cfg.Incus.Pool, driver)
	}
	return nil
}

// TransferHostSupported reports whether this host can take part in a transfer
// at all. It is the receiving side's first question, asked before anything is
// fetched, so a host the feature does not cover says so without touching the
// network.
func (m *Manager) TransferHostSupported() error {
	return m.requireSupportedDriver()
}

// TransferEstimate returns the number of bytes the container's root disk
// currently occupies, used to check the temp file will fit before a long export
// is started, and to tell the far side how much room to find.
func (m *Manager) TransferEstimate(name string) (int64, error) {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return 0, err
	}
	return m.transferDiskUsed(u), nil
}

// transferDiskUsed falls back to the user's disk quota when the daemon has no
// exact figure (a stopped container on some storage drivers reports none).
func (m *Manager) transferDiskUsed(u *db.User) int64 {
	if n, err := m.lx.DiskUsage(u.Name); err == nil && n > 0 {
		return n
	}
	if all, err := m.lx.Metrics(); err == nil {
		if v, ok := all[u.Name]; ok && v.FilesystemSize > 0 && v.FilesystemAvail <= v.FilesystemSize {
			return v.FilesystemSize - v.FilesystemAvail
		}
	}
	return int64(u.DiskGB) << 30
}

// TransferExport streams the user's stopped container to w and returns the
// manifest of what was written, including the metadata that travels with it.
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
	if err := m.requireSupportedDriver(); err != nil {
		return nil, err
	}
	if err := m.requireStopped(u.Name); err != nil {
		return nil, err
	}
	// An expired account is not migrated. Bringing one up on a new host would
	// mean starting a container the panel has already locked and stopped, and
	// an expiry that has already passed would be recreated as a live account.
	// The operator decides: extend it here first, then move it.
	if IsExpired(u.ExpiresAt, time.Now()) {
		return nil, fmt.Errorf("%s expired on %s — extend it first (vps quota %s --days N), then move it",
			u.Name, expiresDate(u.ExpiresAt), u.Name)
	}
	meta, err := m.buildTransferMeta(u)
	if err != nil {
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
	metaSHA := sha256.Sum256(meta)
	_ = m.db.AddAuditLog(opt.Actor, "transfer.export."+u.Name)
	return &TransferManifest{
		User:     u.Name,
		Bytes:    n,
		SHA256:   hex.EncodeToString(sum.Sum(nil)),
		Meta:     meta,
		MetaSHA:  hex.EncodeToString(metaSHA[:]),
		DiskUsed: m.transferDiskUsed(u),
		Elapsed:  time.Since(started),
	}, nil
}

// buildTransferMeta collects everything about the account that lives in this
// host's panel database rather than inside the container.
func (m *Manager) buildTransferMeta(u *db.User) ([]byte, error) {
	meta := TransferMeta{
		User:        u.Name,
		CPU:         u.CPU,
		MemMB:       u.MemMB,
		DiskGB:      u.DiskGB,
		BandwidthGB: u.BandwidthQuotaGB,
		ExpiresAt:   u.ExpiresAt,
		DiskUsed:    m.transferDiskUsed(u),
		InitScript:  u.InitScript,
	}
	keys, err := m.db.ListSSHKeys(u.ID)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		meta.SSHKeys = append(meta.SSHKeys, MetaKey{Name: k.Name, Key: k.Key, Active: k.Active})
	}
	notes, err := m.db.GetStickyNotes(u.ID)
	if err != nil {
		return nil, err
	}
	meta.StickyNotes = notes
	return json.Marshal(meta)
}

// expiresDate renders a deadline for a message, falling back to the raw value.
func expiresDate(rfc3339 string) string {
	if t, err := time.Parse(time.RFC3339, rfc3339); err == nil {
		return t.Format("2006-01-02 15:04 MST")
	}
	return rfc3339
}

// TransferPrecheck reports whether this host can take the container described
// by meta, before a byte of it is fetched: the storage driver has to be one
// this feature covers, and the pool must not already be 90% full — the same
// rule that guards `vps add`, applied here so a host that cannot take the
// container says so instead of spending an hour downloading it. The disk figure
// comes from the sending host's own measurement, since the archive's size says
// nothing about how much will be unpacked.
func (m *Manager) TransferPrecheck(meta []byte) error {
	var t TransferMeta
	if err := json.Unmarshal(meta, &t); err != nil {
		return fmt.Errorf("unreadable transfer metadata: %w", err)
	}
	if err := m.requireSupportedDriver(); err != nil {
		return err
	}
	usage, err := m.PoolUsage()
	if err != nil {
		return err
	}
	if usage >= 0.9 {
		return fmt.Errorf("storage pool %s is %.0f%% used (>= 90%%), refusing to import %s",
			m.cfg.Incus.Pool, usage*100, t.User)
	}
	return nil
}

// TransferMetaAccount parses the account settings out of the metadata, so the
// CLI can show the operator what it is about to create.
func TransferMetaAccount(meta []byte) (AddOptions, string, error) {
	var t TransferMeta
	if err := json.Unmarshal(meta, &t); err != nil {
		return AddOptions{}, "", fmt.Errorf("unreadable transfer metadata: %w", err)
	}
	return t.Account(), t.User, nil
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

// TransferImport creates the account described by meta from a backup tarball
// read from r and returns its connection details.
//
// The account is created first, exactly as `vps add` would create it — so its
// IP, port block, IPv6 assignment, firewall rules and panel row all come from
// the same code path as any other container — and the disk of the container
// that produced is then replaced by the transferred one. Nothing that existed
// before this call is modified, so any failure is undone by deleting what was
// created.
func (m *Manager) TransferImport(ctx context.Context, name string, r io.Reader, metaBytes []byte, opt TransferOptions) (*TransferResult, error) {
	// No opMu here: Add and Del take it themselves, and this runs the two of
	// them in sequence.
	if err := m.TransferTarget(name); err != nil {
		return nil, err
	}
	if err := m.requireSupportedDriver(); err != nil {
		return nil, err
	}
	var meta TransferMeta
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return nil, fmt.Errorf("unreadable transfer metadata: %w", err)
	}
	started := time.Now()
	counting := &countingReader{r: r}

	account := meta.Account()
	if m.cfg.IPv6ModeEffective() == cfg.IPv6ModePool {
		// On a pool-mode host an address is a per-container choice its operator
		// makes, so an import never takes one: the container arrives as pure
		// IPv4, using no pool address, and one can be assigned later if the
		// owner wants it. What the container was given on the host it came from
		// is irrelevant here.
		account.IPv6Addr = "none"
	}
	created, err := m.Add(name, account)
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

	// The account's own data before anything expensive: it is a handful of DB
	// writes, and doing it first means a problem here costs nothing. The keys
	// are injected into the container further down, by the same call that keeps
	// them in step on any other account.
	if err := m.applyTransferMeta(u, &meta); err != nil {
		return fail(err)
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
	// ...along with fresh hardware addresses, for the same reason and one more:
	// the archive also carries the source's, and importing the same archive
	// twice into one host would otherwise collide ("MAC address ... already
	// defined on another NIC"). They are set here because Incus writes the DHCP
	// reservation that gives a container its static IPv4 when the instance is
	// created, keyed to the address it has at that moment — decide it later and
	// the reservation names the old one, so the container is handed a dynamic
	// address while every rule and record points at the static one.
	fresh, err := freshNICAddrs()
	if err != nil {
		return fail(err)
	}
	if err := m.lx.BackupImport(ctx, u.Name, m.cfg.Incus.Pool, importDevices(devices, m.cfg.Incus.Bridge), fresh, counting); err != nil {
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

	// The transferred disk still carries the source's hostname, machine-id and
	// SSH host keys. Rebuilding those is what makes the container this host's
	// own rather than a copy of the other one: a shared machine-id makes
	// dnsmasq drop DHCPv6 leases, and shared host keys would let two machines
	// claim one identity to every client.
	//
	// The root password is deliberately NOT reset: it lives on the disk, the
	// owner already knows it, and leaving it alone is the whole point of a
	// migration that should feel like nothing happened.
	//
	// image only picks the provisioning path; the rootfs came from another
	// vpsmgr host, so it is always one of the managed images.
	if err := m.Provision(u.Name, m.cfg.Incus.Image, ""); err != nil {
		return fail(fmt.Errorf("provision container: %w", err))
	}
	if err := m.regenerateMachineID(u.Name); err != nil {
		return fail(fmt.Errorf("regenerate machine-id: %w", err))
	}
	if err := m.regenerateSSHHostKeys(u.Name); err != nil {
		return fail(fmt.Errorf("regenerate ssh host keys: %w", err))
	}
	// ...and the source's authorized_keys, or the source could still log in.
	// The owner's own keys are written back by applyUserKeys below.
	m.clearAuthorizedKeys(u.Name)

	// The source's IPv6 stanza is still in the container's network config, and
	// this host's is appended rather than written, so it has to go first — and
	// if the container came from a pool-mode host, its bridged NIC has no DHCP
	// client at all and needs one before anything else.
	if err := m.restoreBridgedIPv4(u); err != nil {
		return fail(fmt.Errorf("restore the container's IPv4 setup: %w", err))
	}
	if err := m.dropSourceIPv6(u); err != nil {
		return fail(fmt.Errorf("clear the source host's ipv6 configuration: %w", err))
	}
	switch {
	case m.usesTwoNICs(u):
		if err := m.ConfigureContainerIPv6(u.Name, u.IPv6Address); err != nil {
			return fail(fmt.Errorf("config container ipv6: %w", err))
		}
		if err := m.WireIPv6Pool(u.Name, u.IPv6Address); err != nil {
			return fail(fmt.Errorf("wire ipv6 pool: %w", err))
		}
	case m.cfg.IPv6ModeEffective() == cfg.IPv6ModePool:
		// Pool mode, no address handed out: the container is pure IPv4 and
		// there is nothing to route.
	default:
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
		IP:        u.IP,
		SSHPort:   u.SSHPort,
		Ports:     UserPorts(u.StartPort, cfg.PortsPerUser),
		Elapsed:   time.Since(started),
		BytesRead: counting.n,
	}, nil
}

// applyTransferMeta restores the account data that lives in the panel database:
// the owner's public keys, their notes, their init script and their deadline.
// It is done while the receiving container is still the empty one Add made, so
// a failure here is cheap — but it never runs the script: an init script
// belongs to a reinstall, and a migration must not re-run whatever it did the
// first time.
func (m *Manager) applyTransferMeta(u *db.User, meta *TransferMeta) error {
	for _, k := range meta.SSHKeys {
		name, key := k.Name, k.Key
		if name == "" {
			name = "imported"
		}
		if _, err := m.db.AddSSHKey(u.ID, name, key, k.Active); err != nil {
			return fmt.Errorf("restore ssh key %q: %w", name, err)
		}
	}
	if meta.StickyNotes != "" {
		if err := m.db.SetStickyNotes(u.ID, meta.StickyNotes); err != nil {
			return fmt.Errorf("restore sticky notes: %w", err)
		}
	}
	if meta.InitScript != "" {
		if err := m.db.UpdateInitScript(u.ID, meta.InitScript); err != nil {
			return fmt.Errorf("restore init script: %w", err)
		}
	}
	// The deadline is carried as the absolute date it was, so the account
	// neither loses nor gains time by moving. An expired account never gets
	// this far: the export refuses to start one.
	if meta.ExpiresAt != "" {
		if _, err := m.SetExpiry(u.Name, meta.ExpiresAt); err != nil {
			return fmt.Errorf("restore the expiry: %w", err)
		}
	}
	return nil
}

// dropSourceIPv6 removes the IPv6 configuration the host this container came
// from left inside it, so that what remains is this host's and nothing else.
//
// The re-key step appends this host's address, routed block and gateway
// neighbour and only checks for its OWN values before appending, so a container
// that arrives from another host still carries that host's address and block.
// Left alone it comes up with two global addresses, one of them belonging to a
// network it is not on, and services can bind to the wrong one. A host with no
// IPv6 at all has to lose the whole lot instead — including the one-shot unit
// the source host installed, which would otherwise put its local route back on
// every boot.
//
// Only IPv6 stanzas are touched; the image's own DHCP setup is kept. Pool mode
// rewrites the file outright and has nothing to drop here.
func (m *Manager) dropSourceIPv6(u *db.User) error {
	has := m.containerHasIPv6(u)
	if m.cfg.IPv6ModeEffective() == cfg.IPv6ModePool && has {
		// Pool mode rewrites the guest's network config outright.
		return nil
	}
	keep := "1"
	ipv6, block, mac := "", "", ""
	if has {
		var err error
		ipv6, err = m.IPv6Addr(u.Name)
		if err != nil {
			return err
		}
		if b, _ := m.IPv6Block(u.Name); b != nil {
			block = b.String()
		}
		mac = m.bridgeMAC()
	} else {
		// No IPv6 here: keep none of it.
		keep = "0"
	}
	script := `set -e
KEEP=` + strconv.Quote(keep) + `
if command -v nmcli >/dev/null 2>&1 && ! systemctl is-active systemd-networkd >/dev/null 2>&1; then
  # RHEL-family keeps its addresses in a NetworkManager profile, not a file.
  if [ "$KEEP" = 0 ]; then
    CONN=$(nmcli -t -f NAME,DEVICE con show 2>/dev/null | awk -F: '$2 == "eth0" {print $1; exit}')
    [ -z "$CONN" ] && CONN=$(nmcli -t -f NAME con show 2>/dev/null | grep -i eth0 | head -1)
    if [ -n "$CONN" ]; then
      # The address and the method have to be cleared together: NetworkManager
      # refuses a method of "manual" with no address left, and refuses an
      # address under a method of "disabled", so only the final state, set in
      # one request, is accepted.
      nmcli con mod "$CONN" ipv6.method disabled ipv6.addresses "" ipv6.gateway "" >/dev/null 2>&1 || true
      nmcli con up "$CONN" >/dev/null 2>&1 || true
    fi
  fi
  exit 0
fi
CFG=/etc/systemd/network/eth0.network
if [ -f "$CFG" ]; then
  awk -v keep_v6="$KEEP" -v keep_addr=` + strconv.Quote(ipv6+"/128") + ` -v keep_block=` + strconv.Quote(block) + ` -v keep_mac=` + strconv.Quote(mac) + ` '
function reset() { hdr=""; body=""; addr=""; dest=""; lladdr=""; lcl=0 }
function keepit() {
  if (hdr == "[Address]" && addr != "") { if (keep_v6 == 0 || addr != keep_addr) return 0 }
  if (hdr == "[Route]" && dest != "") { if (keep_v6 == 0) return 0; if (lcl == 1 && dest != keep_block) return 0 }
  if (hdr == "[Neighbor]" && lladdr != "") { if (keep_v6 == 0 || lladdr != keep_mac) return 0 }
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
fi
# The source host's one-shot route unit belongs to the source host.
rm -f /etc/systemd/system/vpsmgr-ipv6.service /etc/systemd/system/multi-user.target.wants/vpsmgr-ipv6.service
if [ "$KEEP" = 0 ]; then
  ip -6 addr flush dev eth0 scope global 2>/dev/null || true
  ip -6 route flush dev eth0 2>/dev/null || true
fi
systemctl daemon-reload >/dev/null 2>&1 || true
systemctl restart systemd-networkd >/dev/null 2>&1 || true
`
	_, err := m.lx.ExecSH(u.Name, script)
	return err
}

// ensureStaticIPv4 waits for the container to take the IPv4 address its whole
// hosting setup is built around: the panel's records, the host's DNAT rules and
// the user's own notes all name it, so a container that ends up on another
// address is unreachable — better to say so than to hand over a machine nobody
// can connect to.
//
// The address is not configured at boot, it is leased: Incus hands a container
// its static IPv4 from a DHCP reservation keyed to its MAC, so it arrives a
// moment after the container is up (later than Ready means) and has to be
// waited for. If the server is still holding the lease of the container this
// one replaced, one restart is what makes it ask again.
func (m *Manager) ensureStaticIPv4(u *db.User) error {
	if u.IP == "" {
		return nil
	}
	// Which NIC carries the private IPv4 depends on the mode: pool mode puts
	// the public /128 on a routed eth0 and the private address on a bridged
	// eth1, while prefix mode has the single bridged eth0 do both.
	nic := "eth0"
	if m.usesTwoNICs(u) {
		nic = "eth1"
	}
	last := ""
	for attempt := 0; attempt < 2; attempt++ {
		deadline := time.Now().Add(40 * time.Second)
		for {
			out, err := m.lx.ExecSH(u.Name, "ip -4 -o addr show dev "+nic+" 2>/dev/null | awk '{print $4}'")
			if err != nil {
				return err
			}
			last = strings.Join(strings.Fields(out), " ")
			for _, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), u.IP+"/") {
					return nil
				}
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(3 * time.Second)
		}
		if attempt == 0 {
			fmt.Printf("  · %s has not taken %s yet — restarting it once\n", u.Name, u.IP)
			if err := m.lx.Restart(u.Name); err != nil {
				return err
			}
		}
	}
	return fmt.Errorf("%s came up on %s, not on its assigned %s — its port forwarding will not work until it takes that address",
		nic, last, u.IP)
}

// usesTwoNICs reports whether this host lays the container out with two NICs:
// a routed eth0 carrying a public /128 from the pool, and a bridged eth1
// carrying the private IPv4. Only a pool-mode container that was actually given
// an address has that shape. Prefix mode, IPv4-only, and a pool-mode container
// with no address all put both addresses on one bridged eth0.
func (m *Manager) usesTwoNICs(u *db.User) bool {
	return m.cfg.IPv6ModeEffective() == cfg.IPv6ModePool && u.IPv6Address != ""
}

// containerHasIPv6 reports whether this host gives the container a global IPv6
// address of its own.
func (m *Manager) containerHasIPv6(u *db.User) bool {
	if !m.cfg.IPv6Enabled() {
		return false
	}
	if m.cfg.IPv6ModeEffective() == cfg.IPv6ModePool {
		return u.IPv6Address != ""
	}
	return true
}

// restoreBridgedIPv4 gives a container's bridged NIC back the DHCPv4 setup this
// host's containers have.
//
// A container that arrives from a pool-mode host carries the network
// configuration THAT host wrote for it, and there eth0 is a routed NIC with no
// IPv4 at all — the private address lives on eth1. So its eth0 has no DHCP
// client, never asks for the address reserved for it here, and comes up with no
// IPv4: every port forward points at nothing. The file is rewritten with
// exactly what the image ships, and the IPv6 step appends this host's addresses
// on top of it.
//
// A container that already has that setup is left alone.
func (m *Manager) restoreBridgedIPv4(u *db.User) error {
	if m.usesTwoNICs(u) {
		// Its IPv4 is on the second, bridged NIC, which the source cannot have
		// configured differently.
		return nil
	}
	script := `set -e
if command -v nmcli >/dev/null 2>&1 && ! systemctl is-active systemd-networkd >/dev/null 2>&1; then
  # RHEL-family (NetworkManager): the pool-mode layout disabled IPv4 on eth0.
  CONN=$(nmcli -t -f NAME,DEVICE con show 2>/dev/null | awk -F: '$2 == "eth0" {print $1; exit}')
  [ -z "$CONN" ] && CONN=$(nmcli -t -f NAME con show 2>/dev/null | grep -i eth0 | head -1)
  [ -n "$CONN" ] && nmcli con mod "$CONN" ipv4.method auto >/dev/null 2>&1 || true
  [ -n "$CONN" ] && nmcli con up "$CONN" >/dev/null 2>&1 || true
else
  CFG=/etc/systemd/network/eth0.network
  grep -qs '^DHCP=ipv4$' "$CFG" 2>/dev/null && exit 0
  mkdir -p /etc/systemd/network
  cat > "$CFG" <<'EOF'
[Match]
Name=eth0

[Network]
DHCP=ipv4

[DHCPv4]
UseDomains=true
UseMTU=true

[DHCP]
ClientIdentifier=mac
EOF
fi
`
	_, err := m.lx.ExecSH(u.Name, script)
	return err
}

// nicLeftovers are the device options an archive's NIC may carry that this
// host's own specification does not decide. They are cleared during the
// import, because Incus validates a device as a whole: half of a bridged NIC
// left on a routed one fails with "Invalid device option" or with an address
// from a subnet this host has never heard of.
//
// Clearing one of these is a no-op whenever this host's spec does set it (the
// same-mode case), so the list only ever bites on a NIC whose very kind changed.
var nicLeftovers = []string{
	// Addresses, which belong to the host the archive came from.
	"ipv4.address", "ipv6.address", "ipv6.routes",
	// We attach NICs by bridge name, never by managed network.
	"network",
	// Hardening that only exists on a bridged NIC.
	"security.ipv4_filtering", "security.ipv6_filtering",
	"security.port_isolation", "security.mac_filtering",
	// Per-instance link identity and settings; the fresh hardware address is
	// supplied separately.
	"host_name", "hwaddr", "mtu", "vlan",
}

// importDevices is the device map an archive is overridden with while it is
// being imported: this host's own devices, plus a neutral entry for every NIC
// name a vpsmgr container can have.
//
// The extras are what let a container move between IPv6 modes. An archive from
// a pool-mode host carries an eth1 that a prefix-mode host has no use for, and
// the override can set device keys but never remove a device — so the spare NIC
// is at least made valid here (bridged, no addresses, nothing to validate
// against a network it does not belong to) and disappears for good when the
// configuration is replaced wholesale right afterwards.
func importDevices(spec map[string]lx.Device, bridge string) map[string]lx.Device {
	out := make(map[string]lx.Device, len(spec)+2)
	for name, dev := range spec {
		clone := make(lx.Device, len(dev))
		for k, v := range dev {
			clone[k] = v
		}
		out[name] = clone
	}
	for _, nic := range []string{"eth0", "eth1"} {
		dev, ok := out[nic]
		if !ok {
			dev = lx.Device{"type": "nic", "nictype": "bridged", "parent": bridge, "name": nic}
			out[nic] = dev
		}
		for _, key := range nicLeftovers {
			if _, decided := dev[key]; !decided {
				dev[key] = ""
			}
		}
	}
	return out
}

// freshNICAddrs returns a new hardware address for each NIC name a vpsmgr
// container can have. Setting one for a NIC the container turns out not to have
// is harmless: it is just an unused configuration key.
func freshNICAddrs() (map[string]string, error) {
	out := map[string]string{}
	for _, nic := range []string{"eth0", "eth1"} {
		mac, err := lx.RandomMAC()
		if err != nil {
			return nil, err
		}
		out["volatile."+nic+".hwaddr"] = mac
	}
	return out, nil
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
