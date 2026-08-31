package mgr

import (
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
	"vpsmgr/internal/fw"
	"vpsmgr/internal/lx"
	"vpsmgr/internal/pw"
	"vpsmgr/internal/su"
	"vpsmgr/internal/tfx"
)

// nameRe is the strict rule for NEW usernames: lowercase letters, digits and
// hyphens, but it must both start and end with a lowercase letter (no
// leading/trailing hyphen, no leading digit). Max 31.
var nameRe = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z])?$`)

// legacyNameRe is the union of every historically-valid username shape, used to
// sanity-check the name on operations that act on an EXISTING user
// (del/quota/passwd). Enforcing the stricter ValidateName there would lock out a
// legacy username that ends in a digit (created before hyphens/end-letter were
// decided). The authoritative existence check is GetUserByName.
var legacyNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

type Manager struct {
	cfg *cfg.Config
	db  *db.DB
	lx  *lx.Client
	fw  *fw.Firewall
	tfx *tfx.Traefik

	// opMu serializes Add/Del/Reinstall within this process (the panels share
	// one Manager), so two simultaneous creates cannot race for the same
	// index/IP. Cross-process CLI races are still caught by the users.idx
	// UNIQUE constraint plus Add's rollback.
	opMu sync.Mutex

	// throttled tracks which containers currently carry a bandwidth throttle
	// (name -> on), so EnforceBandwidthLimits only touches Incus on state changes.
	// limitMu guards it: the 60s sampler writes, the panel reads via
	// IsThrottled. Re-applying a limit after a panel restart is harmless (Incus
	// applies NIC limits live, no container restart).
	limitMu   sync.Mutex
	throttled map[string]bool

	// sampleMu serializes the combined resource sampler. Incus counters are read
	// and the DB delta applied in one critical section so concurrent samplers
	// cannot double-count a counter.
	sampleMu sync.Mutex

	// hostMu protects the latest in-memory host overview. Host history is not
	// persisted; caching only avoids an Incus storage-pool request per page.
	hostMu    sync.RWMutex
	hostStats HostStats
	hostReady bool

	// domainMu serializes domain mutations (AddDomain/DelDomain/
	// SetDomainProtocol + SyncAllDomains) so two panel requests cannot race on
	// the same domain's DB row and YAML file (review P2-12). SyncAllDomains is
	// deliberately NOT held while Del removes domain files — Del already holds
	// opMu, and holding both would risk lock-order deadlock with AddDomain.
	domainMu sync.Mutex
}

func New(c *cfg.Config, d *db.DB) *Manager {
	m := &Manager{cfg: c, db: d, lx: lx.New(c.Incus.Socket, c.Incus.SwapRatio), fw: fw.New(c), tfx: tfx.New(c)}
	m.RefreshHostStats()
	return m
}

func ValidateName(name string) error {
	if len(name) > 31 || !nameRe.MatchString(name) {
		return errors.New("invalid name: must start and end with a lowercase letter, lowercase letters/digits/hyphens only, max 31, no leading/trailing hyphen")
	}
	return nil
}

// ValidateExistingName checks a name against every shape a user could legally
// have been created with (the current strict rule and the pre-hyphen,
// digit-ending one). Used by del/quota/passwd so a legacy user stays operable
// even if their name now fails the stricter creation rule.
func ValidateExistingName(name string) error {
	if len(name) > 31 || !legacyNameRe.MatchString(name) {
		return errors.New("invalid name")
	}
	return nil
}

// UserPorts returns the whole-hundred block of ports a user can bind for
// their own services: start .. start+perUser-1. The SSH port is a separate
// random port (30000-31999), so the entire block is usable. Returns "" when
// perUser < 1.
func UserPorts(start, perUser int) string {
	if perUser < 1 {
		return ""
	}
	end := start + perUser - 1
	if start == end {
		return fmt.Sprintf("%d", start)
	}
	return fmt.Sprintf("%d-%d", start, end)
}

// UserPortsShort renders the user port block in the compact "107xx" form
// (blocks are always whole-hundred aligned) for tight table columns. The full
// range is available via UserPorts.
func UserPortsShort(start int) string {
	return strconv.Itoa(start/100) + "xx"
}

// ContainerIP returns a container's static IPv4 for index idx (1-based) inside
// subnet: the host part is idx+1 (idx=1 -> .2; the gateway is .1). The scheme
// is fixed at /24, so subnets of any other length are rejected.
func ContainerIP(subnet string, idx int) (string, error) {
	_, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return "", err
	}
	ip := ipnet.IP.To4()
	if ip == nil {
		return "", fmt.Errorf("subnet %s is not IPv4", subnet)
	}
	if ones, bits := ipnet.Mask.Size(); ones != 24 || bits != 32 {
		return "", fmt.Errorf("subnet %s is not a /24", subnet)
	}
	if idx < 1 || idx > cfg.MaxUsers {
		return "", fmt.Errorf("idx %d out of range 1..%d", idx, cfg.MaxUsers)
	}
	ip[3] = byte(idx + 1)
	return ip.String(), nil
}

// allocSSHPort picks a random free port from the SSH range. It tries random
// picks first, then falls back to the lowest free one; the ssh_port UNIQUE
// constraint in the DB is the backstop against a cross-process race (the
// in-process opMu already serializes adds).
func (m *Manager) allocSSHPort() (int, error) {
	used, err := m.db.UsedSSHPorts()
	if err != nil {
		return 0, err
	}
	if len(used) >= cfg.SSHPortCount {
		return 0, errors.New("no free ssh port (pool exhausted)")
	}
	for i := 0; i < 32; i++ {
		n, err := rand.Int(rand.Reader, big.NewInt(cfg.SSHPortCount))
		if err != nil {
			break
		}
		p := cfg.SSHPortBase + int(n.Int64())
		if !used[p] {
			return p, nil
		}
	}
	for p := cfg.SSHPortBase; p < cfg.SSHPortBase+cfg.SSHPortCount; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, errors.New("no free ssh port")
}

// PoolUsage returns the used ratio (0..1) of the storage pool as reported by
// Incus. The error is returned to the caller instead of being swallowed: a
// capacity check that cannot read the pool must fail closed (refuse to create),
// never pass through as "0% used".
func (m *Manager) PoolUsage() (float64, error) {
	total, used, err := m.lx.PoolResources(m.cfg.Incus.Pool)
	if err != nil {
		return 0, fmt.Errorf("storage pool %s: %w", m.cfg.Incus.Pool, err)
	}
	if total <= 0 {
		return 0, fmt.Errorf("storage pool %s: Incus reported total size %d", m.cfg.Incus.Pool, total)
	}
	if used > total {
		used = total
	}
	return float64(used) / float64(total), nil
}

// imageName returns the prebuilt image alias if it exists, else the fallback.
func (m *Manager) imageName() (string, error) {
	ok, _ := m.lx.ImageExists(m.cfg.Incus.Image)
	if ok {
		return m.cfg.Incus.Image, nil
	}
	return m.cfg.Incus.ImageFallback, nil
}

// rootPassScript sets the container root password via chpasswd. The password
// is always generated from [a-zA-Z0-9] (pw.Generate) — no user-supplied value
// ever reaches this string, so the single-quoted interpolation cannot break
// out of the shell command.
func rootPassScript(pass string) string {
	return fmt.Sprintf("printf 'root:%s\\n' | chpasswd\n", pass)
}

// randomHostname returns a random, non-revealing hostname for a container
// (e.g. "vps-3fa9c2b1"), drawn from crypto/rand. It never contains the
// username, so users can't identify each other from prompts/logs/banners on
// the internal network, and it is re-rolled on every install/reinstall.
func randomHostname() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("vps-%08x", time.Now().UnixNano()&0xffffffff)
	}
	return fmt.Sprintf("vps-%08x", b)
}

// Provision sets the root password and ensures sshd is running. It also gives
// the container a random hostname (never the username). A prebuilt managed
// image (any `vpsmgr/*` alias, Debian or RHEL-family) only needs a light setup;
// otherwise sshd is installed on the fly (Debian fallback only).
func (m *Manager) Provision(name, image, pass string) error {
	host := randomHostname()
	// hostSetup: apply the hostname live + persist it, and stop cloud-init
	// (present in the distro images) from resetting it back to the instance
	// name (= username) on the next boot.
	hostSetup := `
# Wait for systemd to be ready: right after boot /run/systemd/private may not
# exist yet, making hostnamectl/systemctl fail with "Failed to connect to
# system scope bus". Bounded, cheap.
for i in $(seq 1 40); do
  [ -S /run/systemd/private ] && break
  sleep 0.5
done
VPSMGR_HOST='` + host + `'
printf '%s\n' "$VPSMGR_HOST" > /etc/hostname
hostname "$VPSMGR_HOST" 2>/dev/null || true
hostnamectl set-hostname "$VPSMGR_HOST" 2>/dev/null || true
sed -i "s/^127\.0\.1\.1.*/127.0.1.1 $VPSMGR_HOST/" /etc/hosts 2>/dev/null || true
mkdir -p /etc/cloud/cloud.cfg.d
printf 'preserve_hostname: true\n' > /etc/cloud/cloud.cfg.d/99-vpsmgr-hostname.cfg
`
	if strings.HasPrefix(image, "vpsmgr/") {
		// Prebuilt image: only hostname + root password + make sure sshd runs.
		// The service is `sshd` on RHEL-family and `ssh` on Debian, so try
		// both. The readiness probe already confirmed the container is up.
		script := hostSetup + rootPassScript(pass) + `
if command -v sshd >/dev/null 2>&1; then
  systemctl is-active sshd >/dev/null 2>&1 || systemctl start sshd >/dev/null 2>&1 || systemctl start ssh >/dev/null 2>&1 || true
  systemctl enable sshd >/dev/null 2>&1 || systemctl enable ssh >/dev/null 2>&1 || true
fi`
		_, err := m.lx.ExecSH(name, script)
		return err
	}
	script := hostSetup + rootPassScript(pass) + `
export DEBIAN_FRONTEND=noninteractive
if ! command -v sshd >/dev/null 2>&1; then
  apt-get update -qq
  apt-get install -y -qq openssh-server
fi
mkdir -p /etc/ssh/sshd_config.d
printf 'PermitRootLogin yes\nPasswordAuthentication yes\n' > /etc/ssh/sshd_config.d/99-vpsmgr.conf
systemctl enable ssh >/dev/null 2>&1 || true
systemctl restart ssh`
	_, err := m.lx.ExecSH(name, script)
	return err
}

type AddOptions struct {
	CPU    int
	MemMB  int
	DiskGB int
	// BandwidthGB is the monthly bandwidth quota in GiB (0 = unlimited).
	BandwidthGB int
	// IPv6Addr is the pool-mode address to assign ("" = auto-pick the first
	// free pool address; "none" = create a V4-only container without IPv6).
	// Ignored unless the config is in pool mode (prefix mode always assigns
	// the deterministic derived address).
	IPv6Addr string
}

func (m *Manager) Add(name string, opt AddOptions) (*Result, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	if err := ValidateName(name); err != nil {
		return nil, err
	}
	if err := ValidateCPU(opt.CPU); err != nil {
		return nil, err
	}
	if opt.MemMB < 64 {
		return nil, errors.New("memory must be >= 64 MiB")
	}
	if opt.DiskGB < 1 {
		return nil, errors.New("disk must be >= 1 GiB")
	}
	if _, err := m.db.GetUserByName(name); err == nil {
		return nil, errors.New("user already exists: " + name)
	}
	// Passwords are always generated ([a-zA-Z0-9]) — no user-supplied password
	// is ever accepted, so nothing untrusted reaches the provisioning shell
	// scripts.
	pass := pw.Generate(20)
	hash, err := pw.Hash(pass)
	if err != nil {
		return nil, err
	}
	idMin, idMax := m.cfg.SlotIdxBounds()
	idx, err := m.db.NextFreeIdx(idMin, idMax)
	if err != nil {
		return nil, err
	}
	usage, err := m.PoolUsage()
	if err != nil {
		return nil, err
	}
	if usage >= 0.9 {
		return nil, fmt.Errorf("storage pool %s is %.0f%% used (>= 90%%), refusing to create", m.cfg.Incus.Pool, usage*100)
	}
	image, err := m.imageName()
	if err != nil {
		return nil, err
	}
	ip, err := ContainerIP(m.cfg.Net.Subnet, idx)
	if err != nil {
		return nil, err
	}
	sshPort, err := m.allocSSHPort()
	if err != nil {
		return nil, err
	}
	startPort := cfg.UserPortBase + (idx-1)*cfg.PortsPerUser
	// IPv6 assignment depends on the mode:
	//   - prefix: deterministic /112-derived address (existing behavior)
	//   - pool:   explicit choice, first free pool address, or "" when the
	//             pool is exhausted / the caller opted out with "none"
	//   - none:   no IPv6 at all
	poolAddr := ""
	ipv6 := "" // eth0 ipv6.address handed to Incus (prefix mode only)
	blockStr := ""
	switch m.cfg.IPv6ModeEffective() {
	case cfg.IPv6ModePool:
		if opt.IPv6Addr == "none" {
			ipv6 = ""
		} else {
			a, err := m.pickPoolIPv6(opt.IPv6Addr)
			if err != nil {
				return nil, err
			}
			poolAddr = a
		}
		// Pool mode: the address is NOT set on the Incus eth0 device — Incus
		// rejects a static ipv6.address outside the bridge's subnet. The
		// container binds its /128 itself (ConfigureContainerIPv6) and the host
		// routes it (WireIPv6Pool).
	default:
		ipv6, _ = m.IPv6Addr(name)
		block, _ := m.IPv6Block(name)
		if block != nil {
			blockStr = block.String()
		}
		if err := m.checkIPv6BlockCollision(name, block); err != nil {
			return nil, err
		}
	}
	// Defend against orphan containers: a crashed create (or an out-of-band
	// `incus` instance) could already hold this name or the IP NextFreeIdx just
	// gave us. Refuse rather than create a bridge IP conflict.
	if err := m.checkIncusConflict(name, ip); err != nil {
		return nil, err
	}
	// Make sure the image is present locally (a remote-qualified fallback like
	// "images:debian/13" is pulled first; the API cannot auto-fetch it inside
	// the create call the way the old `incus launch` CLI did).
	if err := m.lx.EnsureImage(image); err != nil {
		return nil, fmt.Errorf("ensure image %s: %w", image, err)
	}
	if err := m.lx.Launch(m.cfg.Incus.Pool, m.cfg.Incus.Bridge, name, image, ip, ipv6, blockStr, poolAddr, m.cfg.Net.ExtIF, opt.CPU, opt.MemMB, opt.DiskGB); err != nil {
		return nil, fmt.Errorf("launch container: %w", err)
	}
	// From here on any failure must roll the container and its host-side
	// plumbing back. Cleanup distinguishes a SUCCESSFUL rollback (delete the DB
	// record, resources are reusable) from a FAILED one (keep the record marked
	// 'failed' so the operator can see the orphan instead of a silent leak —
	// deleting the row would make the leftover container/IP invisible and let
	// NextFreeIdx hand them out again).
	var createdID int64
	cleanup := func() {
		unwireErr := m.UnwireIPv6(name)
		delErr := m.lx.Delete(name)
		fwErr := m.fw.RemoveUser(name)
		relErr := m.fw.Reload()
		if createdID != 0 {
			if delErr != nil {
				// Orphan container left behind: keep the row, mark it failed.
				_ = m.db.UpdateUserStatus(createdID, db.StatusFailed)
				return
			}
			if fwErr != nil || relErr != nil || unwireErr != nil {
				// Container is gone but the firewall/NDP may still reference it.
				// Keep the row (marked failed) until a repair pass cleans up.
				_ = m.db.UpdateUserStatus(createdID, db.StatusFailed)
				return
			}
			_ = m.db.DeleteUser(createdID)
		}
	}
	// Enable ZFS recursive-rollback restore on the root volume so restoring to
	// an older checkpoint auto-discards the later ones (time machine). No-op on
	// non-ZFS pools. On failure the fresh container is rolled back too.
	if err := m.ensureZfsRollbackVolume(name); err != nil {
		cleanup()
		return nil, fmt.Errorf("zfs snapshot rollback setup: %w", err)
	}
	if err := m.Provision(name, image, pass); err != nil {
		cleanup()
		return nil, fmt.Errorf("provision container: %w", err)
	}
	// IPv6 pass-through. Pool mode: the container binds its /128 itself
	// (ConfigureContainerIPv6) and the host routes + proxy_ndp it
	// (WireIPv6Pool). Prefix mode: the /112 NDP proxy rule, then the
	// host-routed peer IPv6 container script. No-op when IPv6 is disabled.
	if m.cfg.IPv6ModeEffective() == cfg.IPv6ModePool {
		if poolAddr != "" {
			if err := m.ConfigureContainerIPv6(name, poolAddr); err != nil {
				cleanup()
				return nil, fmt.Errorf("config container ipv6: %w", err)
			}
			if err := m.WireIPv6Pool(name, poolAddr); err != nil {
				cleanup()
				return nil, fmt.Errorf("wire ipv6 pool: %w", err)
			}
		}
	} else {
		if err := m.WireIPv6(name); err != nil {
			cleanup()
			return nil, fmt.Errorf("wire ipv6: %w", err)
		}
		// Host-routed peer IPv6 (no L2 discovery / MITM between containers).
		if err := m.ConfigureContainerIPv6(name, ""); err != nil {
			cleanup()
			return nil, fmt.Errorf("config container ipv6: %w", err)
		}
	}
	u, err := m.db.CreateUserFull(name, hash, ip, idx, sshPort, startPort, opt.CPU, opt.MemMB, opt.DiskGB, opt.BandwidthGB, db.StatusCreating, poolAddr)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("db: %w", err)
	}
	createdID = u.ID
	if m.cfg.Net.V4Forward {
		if err := m.fw.WriteUser(name, u.IP, u.SSHPort, u.StartPort, cfg.PortsPerUser); err != nil {
			cleanup()
			return nil, fmt.Errorf("write nft rules: %w", err)
		}
	}
	if err := m.fw.Reload(); err != nil {
		cleanup()
		return nil, err
	}
	// The container and all host-side plumbing exist now — the user is live.
	if err := m.db.UpdateUserStatus(u.ID, db.StatusReady); err != nil {
		// The container is fully working; a status write failure must not
		// roll it back. Mark it failed so the operator can see the anomaly
		// instead of silently keeping a 'creating' user forever.
		m.db.UpdateUserStatus(u.ID, db.StatusFailed)
		return nil, fmt.Errorf("db: mark user ready: %w", err)
	}
	return m.ResultFor(u, pass), nil
}

// checkIPv6BlockCollision refuses a new container if its deterministic /112
// block already belongs to another user, or if the block would contain the
// bridge gateway address (the container could then bind the gateway and break
// routing for everyone). No-op when IPv6 is disabled.
func (m *Manager) checkIPv6BlockCollision(name string, block *net.IPNet) error {
	if block == nil {
		return nil
	}
	users, err := m.db.ListUsers()
	if err != nil {
		return err
	}
	blockStr := block.IP.String()
	for _, u := range users {
		if u.Name == name {
			continue
		}
		b, err := m.IPv6Block(u.Name)
		if err != nil {
			return err
		}
		if b != nil && b.IP.String() == blockStr {
			return fmt.Errorf("ipv6 block %s already assigned to user %q (hash collision); choose another name", block.String(), u.Name)
		}
	}
	if n, err := m.cfg.IPv6Network(); err == nil {
		if gw, err := m.bridgeGateway(n); err == nil {
			if gwIP := net.ParseIP(gw); gwIP != nil && block.Contains(gwIP) {
				return fmt.Errorf("ipv6 block %s would contain the bridge gateway %s; choose another name", block.String(), gw)
			}
		}
	}
	return nil
}

// checkIncusConflict refuses to create a container whose name or static IPv4 is
// already claimed by a live Incus instance. This only fires on orphans — a
// crashed add that left a container behind, or an out-of-band `incus` instance —
// because DB users are excluded by NextFreeIdx beforehand.
func (m *Manager) checkIncusConflict(name, ip string) error {
	ips, err := m.lx.InstanceStaticIPs()
	if err != nil {
		return err
	}
	for n, v := range ips {
		if n == name {
			return fmt.Errorf("container name %q already exists in Incus (orphan?); choose another name", name)
		}
		if v == ip {
			return fmt.Errorf("IPv4 %s already assigned to live container %q (orphan?); choose another name", ip, n)
		}
	}
	return nil
}

type Result struct {
	User         *db.User
	Password     string
	PublicIP     string
	State        string
	Domains      []string
	PortsPerUser int
	CPUUse       string
	MemUse       string
	UpGB         string
	DownGB       string
	IPv6         string // primary global address (the one to connect to)
	IPv6Block    string // the /112 block the container owns (informational)
	V4Forward    bool   // whether IPv4 inbound (ssh/ports/domains) is live
}

func (m *Manager) ResultFor(u *db.User, pass string) *Result {
	st, _ := m.lx.State(u.Name)
	return m.resultForState(u, pass, st)
}

func (m *Manager) resultForState(u *db.User, pass, state string) *Result {
	domains, _ := m.db.ListDomains(u.ID)
	ds := make([]string, len(domains))
	for i, d := range domains {
		ds[i] = d.Domain
	}
	up, down := m.BandwidthFor(u.ID)
	// IPv6 address: pool mode shows the DB-stored assignment; prefix mode
	// derives the deterministic address on the fly.
	ipv6 := ""
	block := ""
	if m.cfg.IPv6ModeEffective() == cfg.IPv6ModePool {
		ipv6 = u.IPv6Address
	} else {
		ipv6, _ = m.IPv6Addr(u.Name)
		if b, _ := m.IPv6Block(u.Name); b != nil {
			block = b.String()
		}
	}
	return &Result{User: u, Password: pass, PublicIP: m.cfg.DisplayIP(),
		State: state, Domains: ds, PortsPerUser: cfg.PortsPerUser,
		UpGB: FormatGB(up), DownGB: FormatGB(down), IPv6: ipv6, IPv6Block: block,
		V4Forward: m.cfg.Net.V4Forward}
}

func (m *Manager) Del(name string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	// Acts on an existing user, so use the legacy-compatible check: a strict
	// creation rule must not lock out a pre-hyphen digit-ending username.
	if err := ValidateExistingName(name); err != nil {
		return err
	}
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return err
	}
	// Domain configs are removed FIRST, before the container and DB row. A
	// leftover traefik YAML keeps proxying to the (about to be deleted) IP;
	// if that IP is later reused by a new user, the old domain would silently
	// point at the new tenant (cross-tenant hijack, review P1-7). So a failed
	// domain-file removal aborts the whole delete; the retry re-runs it.
	domains, _ := m.db.ListDomains(u.ID)
	for _, d := range domains {
		if err := m.tfx.RemoveDomain(d.Domain); err != nil {
			return fmt.Errorf("remove traefik config for %s: %w (retry after fixing)", d.Domain, err)
		}
	}
	// If the container cannot actually be removed, keep the DB record and let
	// the admin retry. Deleting the row anyway would orphan the container and
	// let NextFreeIdx reuse its IP/ports for a new user — a bridge IP conflict.
	if err := m.lx.Delete(name); err != nil {
		return fmt.Errorf("delete container: %w", err)
	}
	// The remaining host-side cleanup is best-effort: leftover nft rules
	// without a container are harmless and re-runnable on retry.
	if err := m.fw.RemoveUser(name); err != nil {
		fmt.Printf("  ! warn: remove nft rules: %v\n", err)
	}
	if err := m.fw.Reload(); err != nil {
		fmt.Printf("  ! warn: reload nft: %v\n", err)
	}
	if m.cfg.IPv6ModeEffective() == cfg.IPv6ModePool {
		if err := m.UnwireIPv6Pool(u.Name, u.IPv6Address); err != nil {
			fmt.Printf("  ! warn: unwire ipv6: %v\n", err)
		}
	} else {
		if err := m.UnwireIPv6(u.Name); err != nil {
			fmt.Printf("  ! warn: unwire ipv6: %v\n", err)
		}
	}
	if err := m.db.DeleteUser(u.ID); err != nil {
		return err
	}
	if err := m.db.DeleteSessionsForUser(u.ID); err != nil {
		return err
	}
	m.limitMu.Lock()
	delete(m.throttled, name)
	m.limitMu.Unlock()
	return nil
}

// ApplyV4State enforces the current v4_forward policy: it rewrites (when on)
// or removes (when off) every user's DNAT rules, reloads the ruleset, and
// applies the related Traefik state. Called by
// `vps config set net.v4_forward` and at the end of `vps install`. It also
// records the effective policy in the DB settings so the long-running panel
// process reflects the toggle without a restart.
func (m *Manager) ApplyV4State() error {
	users, err := m.db.ListUsers()
	if err != nil {
		return err
	}
	for _, u := range users {
		if m.cfg.Net.V4Forward {
			if err := m.fw.WriteUser(u.Name, u.IP, u.SSHPort, u.StartPort, cfg.PortsPerUser); err != nil {
				return err
			}
		} else {
			if err := m.fw.RemoveUser(u.Name); err != nil {
				return err
			}
		}
	}
	if err := m.fw.Reload(); err != nil {
		return err
	}
	if err := m.db.SetSetting(db.SettingV4Forward, strconv.FormatBool(m.cfg.Net.V4Forward)); err != nil {
		return fmt.Errorf("record v4_forward: %w", err)
	}
	return m.ApplyTraefikState()
}

// V4ForwardLive reports whether IPv4 inbound is currently enabled, preferring
// the DB setting (written by ApplyV4State) over the manager's in-memory config.
// The panel process reads its config only at startup, so without this it would
// keep serving domains for a v4_forward toggle made by `vps config set`.
func (m *Manager) V4ForwardLive() bool {
	v, ok, err := m.db.GetSetting(db.SettingV4Forward)
	if err != nil || !ok {
		return m.cfg.Net.V4Forward
	}
	return v == "1" || strings.EqualFold(v, "true")
}

// TraefikLive reports the effective domain-proxy toggle, including changes
// made by `vps config set` while this panel process remains running.
func (m *Manager) TraefikLive() bool {
	v, ok, err := m.db.GetSetting(db.SettingTraefik)
	if err != nil || !ok {
		return m.cfg.Net.Traefik
	}
	return v == "1" || strings.EqualFold(v, "true")
}

// ApplyTraefikState starts/stops the traefik service to match v4_forward and
// net.traefik: with either disabled, the domain proxy is not offered, so
// traefik is stopped and
// its BOOT AUTOSTART is disabled (systemctl disable --now) — it must not come
// back on the next reboot. Domain config files are KEPT, so re-enabling
// restores them; a full re-sync runs when enabling. systemctl errors are
// surfaced, not swallowed.
func (m *Manager) ApplyTraefikState() error {
	if err := m.db.SetSetting(db.SettingTraefik, strconv.FormatBool(m.cfg.Net.Traefik)); err != nil {
		return fmt.Errorf("record traefik: %w", err)
	}
	if m.cfg.Net.V4Forward && m.cfg.Net.Traefik {
		if err := systemctl("enable", "--now", "traefik.service"); err != nil {
			return fmt.Errorf("start traefik: %w", err)
		}
		return m.SyncAllDomains()
	}
	if err := systemctl("disable", "--now", "traefik.service"); err != nil {
		return fmt.Errorf("stop/disable traefik: %w", err)
	}
	return nil
}

// systemctl runs systemctl and returns its stderr on failure. The panel
// daemon is unprivileged, so traefik (and self) control goes through the
// sudoers whitelist.
func systemctl(args ...string) error {
	if _, err := su.Run(append([]string{"/usr/bin/systemctl"}, args...)...); err != nil {
		return err
	}
	return nil
}

// SyncAllDomains reconciles the traefik dynamic directory against the DB (the
// single source of truth): it regenerates every domain's YAML and removes any
// leftover file that no longer matches a domain. This is the safety net that
// fixes any drift left by a crash between a DB write and a file write.
func (m *Manager) SyncAllDomains() error {
	m.domainMu.Lock()
	defer m.domainMu.Unlock()
	domains, err := m.db.ListAllDomains()
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(domains))
	var firstErr error
	for _, d := range domains {
		known[d.Domain] = true
		if err := m.tfx.WriteDomain(d.Domain, d.IP, d.ProxyProtocol); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	files, err := m.tfx.ListFiles()
	if err != nil {
		return err
	}
	for _, f := range files {
		if !known[f] {
			if err := m.tfx.RemoveDomain(f); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (m *Manager) List() ([]*Result, error) {
	users, err := m.db.ListUsers()
	if err != nil {
		return nil, err
	}
	latest, _ := m.db.LatestResourceSamples()
	average, _ := m.db.AverageCPU(time.Now().Add(-time.Hour).Unix())
	live := m.liveStatusFallback(users, latest)
	out := make([]*Result, 0, len(users))
	for _, u := range users {
		sample, hasSample := latest[u.ID]
		avg, hasAverage := average[u.ID]
		r := m.resultForState(u, "", resolvedState(sample, live[u.Name]))
		decorateStoredUsage(r, sample, hasSample, avg, hasAverage)
		out = append(out, r)
	}
	return out, nil
}

func (m *Manager) Show(name string) (*Result, error) {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return nil, err
	}
	latest, _ := m.db.LatestResourceSamples()
	average, _ := m.db.AverageCPU(time.Now().Add(-time.Hour).Unix())
	sample, hasSample := latest[u.ID]
	live := m.liveStatusFallback([]*db.User{u}, latest)
	avg, hasAverage := average[u.ID]
	r := m.resultForState(u, "", resolvedState(sample, live[u.Name]))
	decorateStoredUsage(r, sample, hasSample, avg, hasAverage)
	return r, nil
}

func (m *Manager) State(name string) (string, error) { return m.lx.State(name) }

// UpdateQuotas adjusts CPU/mem/disk of an existing user live (values <= 0 are
// left unchanged). The Incus changes are applied first and the DB written last;
// if a later step fails, everything already applied to Incus is rolled back so
// the live config and the database cannot disagree.
func (m *Manager) UpdateQuotas(name string, cpu, memMB, diskGB int) (*Result, error) {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return nil, err
	}
	// Validate everything before touching Incus so a rejected value never leaves
	// a half-applied quota.
	if cpu > 0 {
		if err := ValidateCPU(cpu); err != nil {
			return nil, err
		}
	}
	if diskGB > 0 && diskGB < u.DiskGB {
		return nil, fmt.Errorf("disk can only grow: current %d GiB, cannot shrink to %d GiB", u.DiskGB, diskGB)
	}
	// undo restores the Incus values applied so far, in reverse order. Best
	// effort: if a rollback itself fails (e.g. Incus refusing a disk shrink),
	// the caller still learns about it via the error text.
	var undo []func() error
	fail := func(err error) (*Result, error) {
		var warns []string
		for i := len(undo) - 1; i >= 0; i-- {
			if e := undo[i](); e != nil {
				warns = append(warns, e.Error())
			}
		}
		if len(warns) > 0 {
			return nil, fmt.Errorf("%v (rollback partial: %s)", err, strings.Join(warns, "; "))
		}
		return nil, err
	}
	if cpu > 0 {
		old := u.CPU
		if err := m.lx.SetCPU(u.Name, cpu); err != nil {
			return fail(err)
		}
		u.CPU = cpu
		undo = append(undo, func() error { return m.lx.SetCPU(u.Name, old) })
	}
	if memMB > 0 {
		old := u.MemMB
		if err := m.lx.SetMem(u.Name, memMB); err != nil {
			return fail(err)
		}
		u.MemMB = memMB
		undo = append(undo, func() error { return m.lx.SetMem(u.Name, old) })
	}
	if diskGB > 0 {
		old := u.DiskGB
		if err := m.lx.SetDisk(u.Name, diskGB); err != nil {
			return fail(err)
		}
		u.DiskGB = diskGB
		undo = append(undo, func() error { return m.lx.SetDisk(u.Name, old) })
	}
	if err := m.db.UpdateQuotas(u.ID, u.CPU, u.MemMB, u.DiskGB); err != nil {
		return fail(err)
	}
	return m.ResultFor(u, ""), nil
}

// UpdateQuotasAndBandwidth adjusts CPU/mem/disk and the monthly bandwidth quota
// in one call, so the admin panel's single submit cannot succeed halfway. The
// bandwidth quota is a DB-only write: it is applied first and rolled back if the
// Incus-side resource update fails. bandwidthGB < 0 leaves the bandwidth quota
// unchanged.
func (m *Manager) UpdateQuotasAndBandwidth(name string, cpu, memMB, diskGB, bandwidthGB int) (*Result, error) {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return nil, err
	}
	if bandwidthGB >= 0 {
		if err := m.db.UpdateBandwidthQuota(u.ID, bandwidthGB); err != nil {
			return nil, err
		}
	}
	res, err := m.UpdateQuotas(name, cpu, memMB, diskGB)
	if err != nil {
		if bandwidthGB >= 0 {
			// restore the previous bandwidth quota so the form is fully rolled back
			_ = m.db.UpdateBandwidthQuota(u.ID, u.BandwidthQuotaGB)
		}
		return nil, err
	}
	return res, nil
}

// Power start/stops/restarts a container. boot.autostart mirrors the desired
// state: starting (or restarting) re-enables it so a host reboot brings the
// container back, while stopping disables it so a manually stopped container
// stays off after the host reboots for maintenance.
func (m *Manager) Power(name, action string) error {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return err
	}
	switch action {
	case "start":
		if err := m.lx.SetAutostart(u.Name, true); err != nil {
			return err
		}
		return m.lx.Start(u.Name)
	case "stop":
		if err := m.lx.SetAutostart(u.Name, false); err != nil {
			return err
		}
		return m.lx.Stop(u.Name)
	case "restart":
		if err := m.lx.SetAutostart(u.Name, true); err != nil {
			return err
		}
		return m.lx.Restart(u.Name)
	}
	return errors.New("unknown action")
}

// validSnapName restricts snapshot names to a safe charset so user-supplied
// names can never reach the Incus API path with separators, and to Incus's
// 63-char name limit.
var validSnapName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,62}$`)

// ValidSnapName reports whether a snapshot name is safe to pass to the Incus
// API. Exported for tests and the panel.
func ValidSnapName(v string) bool { return validSnapName.MatchString(v) }

// SnapshotLimit returns the configured per-container snapshot cap. 0 (or a
// negative value, which the config validator forbids anyway) means snapshots
// are disabled: users cannot create new ones, but existing snapshots are left
// untouched. Any positive value is the hard cap.
func (m *Manager) SnapshotLimit() int {
	if m.cfg.Snapshots.Limit < 0 {
		return 0
	}
	return m.cfg.Snapshots.Limit
}

// snapName returns a fresh snapshot name like snap-20260820-153000-4f7a (UTC
// second + 4 random hex chars, so concurrent creates in the same second never
// collide).
func snapName() string {
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return fmt.Sprintf("snap-%s-%04x", time.Now().UTC().Format("20060102-150405"), b)
}

// SnapshotCreate takes a disk-only snapshot of the user's container, enforcing
// the per-container snapshot cap. opMu serializes the count-then-create so two
// concurrent requests cannot both pass the cap check.
func (m *Manager) SnapshotCreate(name string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	u, err := m.db.GetUserByName(name)
	if err != nil {
		return err
	}
	limit := m.SnapshotLimit()
	if limit == 0 {
		return errors.New("snapshots are disabled")
	}
	snaps, err := m.lx.SnapshotList(u.Name)
	if err != nil {
		return err
	}
	if len(snaps) >= limit {
		return fmt.Errorf("snapshot limit reached (%d)", limit)
	}
	return m.lx.SnapshotCreate(u.Name, snapName())
}

// SnapshotList returns the user's container snapshots.
func (m *Manager) SnapshotList(name string) ([]lx.SnapshotInfo, error) {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return nil, err
	}
	return m.lx.SnapshotList(u.Name)
}

// SnapshotDelete removes a snapshot.
func (m *Manager) SnapshotDelete(name, snapName string) error {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return err
	}
	if !ValidSnapName(snapName) {
		return errors.New("invalid snapshot name")
	}
	return m.lx.SnapshotDelete(u.Name, snapName)
}

// SnapshotRestore restores the container disk from a snapshot, keeping the
// current container configuration (quota, NICs, autostart). A snapshot is a
// rollback point, so a manual snapshot behaves like a time machine that only
// goes back: restoring to one that is NOT the newest discards every snapshot
// created after it. If the container was running it is stopped first and
// started again afterwards; a failure in the middle leaves the container
// stopped and returns the error so the caller can see the partial state rather
// than pretending the restore succeeded. opMu prevents a concurrent
// reinstall/delete from racing the restore.
//
// Primary path: a single Incus restore. Every container's root volume carries
// zfs.remove_snapshots=true (set at create and re-applied on every `vps
// install`, including an --update upgrade), so Incus itself auto-destroys the
// newer snapshots — no per-snapshot cleanup. Fallback: on a host that somehow
// still lacks the option, Incus refuses with a "subsequent snapshot" error;
// we then discard the newer snapshots ourselves and retry once.
func (m *Manager) SnapshotRestore(name, snapName string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	u, err := m.db.GetUserByName(name)
	if err != nil {
		return err
	}
	if !ValidSnapName(snapName) {
		return errors.New("invalid snapshot name")
	}
	st, err := m.lx.State(u.Name)
	if err != nil {
		return err
	}
	wasRunning := st == "Running"
	if wasRunning {
		if err := m.lx.Stop(u.Name); err != nil {
			return err
		}
	}
	if err := m.lx.SnapshotRestore(u.Name, snapName); err != nil {
		// Restore to an older checkpoint is refused while later ones exist and
		// the volume lacks zfs.remove_snapshots. Delete them and retry once.
		if !strings.Contains(err.Error(), "subsequent snapshot") {
			return err
		}
		if derr := m.discardNewerSnapshots(u.Name, snapName); derr != nil {
			return derr
		}
		if err := m.lx.SnapshotRestore(u.Name, snapName); err != nil {
			return err
		}
	}
	if wasRunning {
		return m.lx.Start(u.Name)
	}
	return nil
}

// ensureZfsRollbackVolume sets zfs.remove_snapshots on the container's root
// volume so a single restore auto-discards the newer snapshots (see
// lx.EnsureZFSRemoveSnapshots). Called on Add and Reinstall.
func (m *Manager) ensureZfsRollbackVolume(name string) error {
	return m.lx.EnsureZFSRemoveSnapshots(m.cfg.Incus.Pool, name)
}

// discardNewerSnapshots deletes every snapshot created strictly after the
// target (matching Incus's own "subsequent snapshot" notion, judged by
// creation timestamps), so the restore that follows is always a valid
// rollback. The target itself and any older snapshot are kept; a snapshot
// whose time cannot be parsed is left alone (safe degradation). If the target
// is not found we delete nothing and let the restore call report the
// authoritative error.
func (m *Manager) discardNewerSnapshots(container, target string) error {
	snaps, err := m.lx.SnapshotList(container)
	if err != nil {
		return err
	}
	for _, s := range newerSnapshotNames(snaps, target) {
		if err := m.lx.SnapshotDelete(container, s); err != nil {
			return err
		}
	}
	return nil
}

// newerSnapshotNames returns the names of snapshots created strictly after
// target, preserving list order. Timestamps are compared at full precision;
// a snapshot whose creation time cannot be parsed is ignored (safe).
func newerSnapshotNames(snaps []lx.SnapshotInfo, target string) []string {
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
	var newer []string
	for _, s := range snaps {
		if s.Name == target {
			continue
		}
		if t := snapshotCreateTime(s); !t.IsZero() && t.After(targetTime) {
			newer = append(newer, s.Name)
		}
	}
	return newer
}

// snapshotCreateTime parses an Incus snapshot creation timestamp (RFC3339,
// with optional fractional seconds) and returns the zero time if it cannot.
func snapshotCreateTime(s lx.SnapshotInfo) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s.CreatedAt); err == nil {
			return t
		}
	}
	return time.Time{}
}

// ChangePanelPassword updates only the panel login hash and invalidates all
// other sessions of the user. keepToken is the current session token to
// preserve (empty means none, so every session is dropped). Container root
// password is managed separately via ResetRootPassword.
func (m *Manager) ChangePanelPassword(name, pass, keepToken string) error {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return err
	}
	hash, err := pw.Hash(pass)
	if err != nil {
		return err
	}
	if err := m.db.UpdatePassword(u.ID, hash); err != nil {
		return err
	}
	return m.db.DeleteSessionsForUserExcept(u.ID, keepToken)
}

// ResetPanelPassword sets the panel login password to a new random 20-char
// value, drops all existing sessions of the user and returns the password for
// one-time display. Container root password is untouched.
func (m *Manager) ResetPanelPassword(name string) (string, error) {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return "", err
	}
	pass := pw.Generate(20)
	if err := m.ChangePanelPassword(u.Name, pass, ""); err != nil {
		return "", err
	}
	return pass, nil
}

// ResetRootPassword sets the container root password to a new random 20-char
// value and returns it for one-time display. The panel hash is not touched.
func (m *Manager) ResetRootPassword(name string) (string, error) {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return "", err
	}
	image, err := m.imageName()
	if err != nil {
		return "", err
	}
	pass := pw.Generate(20)
	if err := m.Provision(u.Name, image, pass); err != nil {
		return "", err
	}
	return pass, nil
}

// SetInitScript stores a user's custom init script, which is run inside the
// container after a reinstall. Bounded to cfg.MaxInitScriptBytes; an empty
// string clears it.
func (m *Manager) SetInitScript(name, script string) error {
	if len(script) > cfg.MaxInitScriptBytes {
		return fmt.Errorf("init script too large (%d bytes, max %d)", len(script), cfg.MaxInitScriptBytes)
	}
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return err
	}
	return m.db.UpdateInitScript(u.ID, script)
}

// Reinstall destroys and recreates the container keeping IP/ports/domains,
// using the selected OS image. image may be a managed alias ("vpsmgr/...");
// empty or the default alias resolves to Debian 13 (prebuilt, else the remote
// fallback). A picked non-default managed image must exist on the host. A new
// random root password is generated (returned for one-time display); the panel
// login password is unchanged.
func (m *Manager) Reinstall(name, image string) (string, error) {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	u, err := m.db.GetUserByName(name)
	if err != nil {
		return "", err
	}
	// Persistent lifecycle state: mark reinstalling BEFORE destroying the old
	// container so a crash mid-reinstall leaves a visible state instead of a
	// DB row that pretends the container is fine.
	if err := m.db.UpdateUserStatus(u.ID, db.StatusReinstalling); err != nil {
		return "", fmt.Errorf("mark reinstalling: %w", err)
	}
	rollback := func() error {
		// Best-effort removal of the half-built container; the row stays in
		// 'failed' so the operator sees what happened and a repair pass can
		// retry or clean up.
		_ = m.UnwireIPv6(u.Name)
		_ = m.lx.Delete(u.Name)
		_ = m.db.UpdateUserStatus(u.ID, db.StatusFailed)
		return nil
	}
	if err := m.lx.Delete(u.Name); err != nil {
		m.db.UpdateUserStatus(u.ID, db.StatusFailed)
		return "", fmt.Errorf("delete container: %w", err)
	}
	// Default image (or empty): resolve to the configured alias / fallback and
	// pull the fallback if it is remote-qualified. A user-picked non-default
	// image must already exist locally — never auto-fetch a surprise image.
	isDefault := image == "" || image == m.cfg.Incus.Image
	if isDefault {
		image, err = m.imageName()
		if err != nil {
			rollback()
			return "", err
		}
		if err := m.lx.EnsureImage(image); err != nil {
			rollback()
			return "", fmt.Errorf("ensure image %s: %w", image, err)
		}
	} else if ok, _ := m.lx.ImageExists(image); !ok {
		rollback()
		return "", fmt.Errorf("image %s is not available on this host (run the image build script)", image)
	}
	// IPv6: pool mode reuses the DB-stored assignment (the address belongs to
	// the user for life), bound inside the container; prefix mode re-derives
	// the deterministic address on the eth0 device.
	ipv6 := ""
	blockStr := ""
	if m.cfg.IPv6ModeEffective() == cfg.IPv6ModePool {
		// NOT set on the Incus eth0 device (rejected: outside bridge subnet).
	} else {
		ipv6, _ = m.IPv6Addr(u.Name)
		block, _ := m.IPv6Block(u.Name)
		if block != nil {
			blockStr = block.String()
		}
	}
	if err := m.lx.Launch(m.cfg.Incus.Pool, m.cfg.Incus.Bridge, u.Name, image, u.IP, ipv6, blockStr, u.IPv6Address, m.cfg.Net.ExtIF, u.CPU, u.MemMB, u.DiskGB); err != nil {
		rollback()
		return "", fmt.Errorf("recreate container: %w", err)
	}
	// Enable ZFS recursive-rollback restore on the fresh root volume (time
	// machine): a restore to an older checkpoint auto-discards the later ones.
	if err := m.ensureZfsRollbackVolume(u.Name); err != nil {
		rollback()
		return "", fmt.Errorf("zfs snapshot rollback setup: %w", err)
	}
	pass := pw.Generate(20)
	if err := m.Provision(u.Name, image, pass); err != nil {
		rollback()
		return "", fmt.Errorf("provision container: %w", err)
	}
	if m.cfg.IPv6ModeEffective() == cfg.IPv6ModePool {
		if u.IPv6Address != "" {
			if err := m.ConfigureContainerIPv6(u.Name, u.IPv6Address); err != nil {
				rollback()
				return "", fmt.Errorf("config container ipv6: %w", err)
			}
			if err := m.WireIPv6Pool(u.Name, u.IPv6Address); err != nil {
				rollback()
				return "", fmt.Errorf("wire ipv6 pool: %w", err)
			}
		}
	} else {
		if err := m.WireIPv6(u.Name); err != nil {
			rollback()
			return "", fmt.Errorf("wire ipv6: %w", err)
		}
		if err := m.ConfigureContainerIPv6(u.Name, ""); err != nil {
			rollback()
			return "", fmt.Errorf("config container ipv6: %w", err)
		}
	}
	// User-defined init script (if any): run it detached inside the container,
	// last of all, so it sees the full network. Best-effort — the container is
	// already provisioned and usable, so a delivery failure must not fail the
	// reinstall (the script is logged inside the container).
	if u.InitScript != "" {
		if err := m.lx.RunInitScript(u.Name, u.InitScript); err != nil {
			fmt.Printf("  ! warn: init script: %v (container still recreated)\n", err)
		}
	}
	// Active SSH keys (if any): write them into the fresh container, including
	// the operator keys this user has activated, so admin access survives a
	// reinstall. Same best-effort contract — keys persist in the DB and apply
	// on the next save.
	if active, err := m.ActiveKeys(u.Name); err == nil {
		admin, _ := m.GrantedAdminKeys(u.Name)
		if len(active) > 0 || len(admin) > 0 {
			if err := m.ApplySSHKeys(u.Name, active, admin); err != nil {
				fmt.Printf("  ! warn: ssh keys: %v (container still recreated)\n", err)
			}
		}
	}
	// The reinstall is complete: back to ready. Also drop any in-memory
	// throttled flag for this user — the old container's NIC limit died with
	// it, so the next EnforceBandwidthLimits pass must re-evaluate from the
	// actual Incus state instead of believing a stale "already throttled".
	if err := m.db.UpdateUserStatus(u.ID, db.StatusReady); err != nil {
		m.db.UpdateUserStatus(u.ID, db.StatusFailed)
		return "", fmt.Errorf("db: mark user ready: %w", err)
	}
	m.limitMu.Lock()
	delete(m.throttled, u.Name)
	m.limitMu.Unlock()
	return pass, nil
}

// AddDomain binds a domain to a user. DB and traefik YAML are kept in sync
// atomically: insert the DB row, write the domain's YAML file, and if the file
// write fails roll the DB row back so the two never disagree.
func (m *Manager) AddDomain(name, domain string, proxyProtocol bool) error {
	m.domainMu.Lock()
	defer m.domainMu.Unlock()
	if !m.V4ForwardLive() {
		return errors.New("v4 forwarding is disabled (v4_forward: false) — domains are not available; re-enable with `vps config set net.v4_forward true`")
	}
	if !m.TraefikLive() {
		return errors.New("Traefik is disabled (net.traefik: false) — domains are not available; re-enable with `vps config set net.traefik true`")
	}
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return err
	}
	domain, err = normalizeDomain(domain)
	if err != nil {
		return err
	}
	// Refuse domains on the admin blocked list (the entry and all its
	// subdomains). Read fresh on every add so panel edits apply immediately;
	// a list that cannot be read fails closed — never silently allow.
	if blockedBy, err := m.DomainBlocked(domain); err != nil {
		return err
	} else if blockedBy != "" {
		return fmt.Errorf("domain %q is blocked (blocks %q and all its subdomains)", domain, blockedBy)
	}
	if exists, err := m.db.DomainExists(domain); err != nil {
		return err
	} else if exists {
		return errors.New("domain already bound")
	}
	if _, err := m.db.AddDomain(u.ID, domain, proxyProtocol); err != nil {
		return err
	}
	if err := m.tfx.WriteDomain(domain, u.IP, proxyProtocol); err != nil {
		_ = m.db.DeleteDomain(u.ID, domain) // roll back the DB row
		return fmt.Errorf("write traefik config: %w", err)
	}
	return nil
}

// DelDomain unbinds a domain, atomically: delete the DB row, remove the YAML
// file, and if the file removal fails re-insert the row.
func (m *Manager) DelDomain(name, domain string) error {
	m.domainMu.Lock()
	defer m.domainMu.Unlock()
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return err
	}
	domain, err = normalizeDomain(domain)
	if err != nil {
		return err
	}
	dmn, err := m.db.GetDomain(u.ID, domain)
	if err != nil {
		return err
	}
	if err := m.db.DeleteDomain(u.ID, domain); err != nil {
		return err
	}
	if err := m.tfx.RemoveDomain(domain); err != nil {
		if _, rerr := m.db.AddDomain(u.ID, domain, dmn.ProxyProtocol); rerr == nil {
			return fmt.Errorf("remove traefik config: %w", err)
		}
		return err
	}
	return nil
}

// SetDomainProtocol toggles a user's domain PROXY protocol flag, atomically:
// update the DB, rewrite the YAML, and on failure restore the old flag.
func (m *Manager) SetDomainProtocol(name, domain string, on bool) error {
	m.domainMu.Lock()
	defer m.domainMu.Unlock()
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return err
	}
	domain, err = normalizeDomain(domain)
	if err != nil {
		return err
	}
	dmn, err := m.db.GetDomain(u.ID, domain)
	if err != nil {
		return err
	}
	if dmn.ProxyProtocol == on {
		return nil
	}
	if err := m.db.SetDomainProtocol(dmn.ID, on); err != nil {
		return err
	}
	if err := m.tfx.WriteDomain(domain, u.IP, on); err != nil {
		_ = m.db.SetDomainProtocol(dmn.ID, dmn.ProxyProtocol) // restore old flag
		return fmt.Errorf("write traefik config: %w", err)
	}
	return nil
}

// AdminSetDomainProtocol is the admin-panel variant: it looks up the owning
// user by the (globally unique) domain and applies the change.
func (m *Manager) AdminSetDomainProtocol(domain string, on bool) error {
	dmn, err := m.db.GetDomainByDomain(domain)
	if err != nil {
		return err
	}
	u, err := m.db.GetUserByID(dmn.UserID)
	if err != nil {
		return err
	}
	return m.SetDomainProtocol(u.Name, domain, on)
}

// AdminDelDomain is the admin-panel variant of DelDomain.
func (m *Manager) AdminDelDomain(domain string) error {
	dmn, err := m.db.GetDomainByDomain(domain)
	if err != nil {
		return err
	}
	u, err := m.db.GetUserByID(dmn.UserID)
	if err != nil {
		return err
	}
	return m.DelDomain(u.Name, domain)
}

// AllDomains returns every domain with its owner (username/IP) and the
// PROXY protocol flag, newest modification first. For the admin panel.
func (m *Manager) AllDomains() ([]*db.DomainView, error) {
	return m.db.ListAllDomains()
}

// HardenAll applies the NIC isolation options to every existing container.
// Called by `vps install` so an upgrade to an isolated build hardens the
// previously-created containers in place. Idempotent; containers that were
// already isolated (or exist in the DB but not in Incus) are skipped. Skips and
// non-fatal errors are collected, not returned — one stale row must not break
// the rest.
func (m *Manager) HardenAll() error {
	users, err := m.db.ListUsers()
	if err != nil {
		return err
	}
	var firstErr error
	for _, u := range users {
		if _, err := m.lx.HardenIsolation(u.Name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ApplySwapToAll refreshes the swap allowance (limits.memory.swap) of every
// existing container from its current memory limit and the configured
// incus.swap_ratio. Memory limits are untouched. Idempotent — safe to run
// after every swap_ratio change, and the way containers created before swap
// support get a swap allowance at all. Containers not present in Incus are
// skipped; the first error encountered is returned (others are still tried).
func (m *Manager) ApplySwapToAll() error {
	users, err := m.db.ListUsers()
	if err != nil {
		return err
	}
	var firstErr error
	for _, u := range users {
		if err := m.lx.SetSwap(u.Name, u.MemMB); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// ApplyZFSRemoveSnapshotsToAll enables the one-call snapshot restore (time
// machine rollback) on every existing container's root volume. This is the
// upgrade path: `vps install` runs it, so a host updated via `install.sh
// --update` gets the automatic "restore discards the later checkpoints"
// behaviour immediately, without reinstalling containers. No-op on non-ZFS
// pools (dir test backend has no snapshots). Idempotent — volumes already
// configured are skipped by EnsureZFSRemoveSnapshots.
func (m *Manager) ApplyZFSRemoveSnapshotsToAll() error {
	users, err := m.db.ListUsers()
	if err != nil {
		return err
	}
	var firstErr error
	for _, u := range users {
		if err := m.lx.EnsureZFSRemoveSnapshots(m.cfg.Incus.Pool, u.Name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// EnsureBlockRoutes adds the deterministic /112 block (ipv6.routes) to every
// existing container's eth0, so an upgrade to the /112 scheme routes each
// container's whole block. Idempotent; a container without IPv6 (or not in
// Incus) is skipped. Restarts containers that needed the change. Pool mode
// has no /112 blocks — skipped entirely.
func (m *Manager) EnsureBlockRoutes() error {
	if !m.cfg.IPv6Enabled() || m.cfg.IPv6ModeEffective() == cfg.IPv6ModePool {
		return nil
	}
	users, err := m.db.ListUsers()
	if err != nil {
		return err
	}
	var firstErr error
	for _, u := range users {
		b, err := m.IPv6Block(u.Name)
		if err != nil || b == nil {
			continue
		}
		if _, err := m.lx.EnsureEth0Options(u.Name, map[string]string{"ipv6.routes": b.String()}); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// normalizeDomain validates and normalizes a domain for use in Traefik
// dynamic YAML. Rules:
//  1. lowercase, strip surrounding whitespace and ALL trailing dots
//  2. keep only [a-z0-9.-] — any other character is a hard error (never
//     silently stripped), so pasting "https://example.com" or a path is
//     rejected instead of being quietly rewritten
//  3. must end with a letter (this also excludes every dotted-quad IPv4)
//  4. must contain at least one dot (no single-word names)
//  5. every label must start and end with a letter or digit (dots can only be
//     between labels, hyphens never at a label's edge — consecutive hyphens
//     inside a label like "a--b" or "xn--p1ai" are fine)
//  6. total length ≤ 253, every label ≤ 63
//
// No TLD validation. The resulting string is safe to drop into YAML and as a
// filename (dots/hyphens only).
func normalizeDomain(d string) (string, error) {
	d = strings.TrimSpace(strings.ToLower(d))
	d = strings.TrimRight(d, ".")
	if d == "" {
		return "", errors.New("domain empty")
	}
	if len(d) > 253 {
		return "", errors.New("domain too long (max 253 characters)")
	}
	for i := 0; i < len(d); i++ {
		if !isDomainChar(d[i]) && d[i] != '.' && d[i] != '-' {
			return "", errors.New("invalid domain: only lowercase letters, digits, dots and hyphens are allowed")
		}
	}
	if !strings.Contains(d, ".") {
		return "", errors.New("invalid domain: must be a full domain name (e.g. example.com), not a single word")
	}
	if last := d[len(d)-1]; last < 'a' || last > 'z' {
		return "", errors.New("invalid domain: must end with a letter")
	}
	// Every label must be non-empty and start/end with a letter or digit.
	// This rejects leading/trailing/consecutive dots and any hyphen at a
	// label's edge, while allowing consecutive hyphens mid-label.
	for _, label := range strings.Split(d, ".") {
		if label == "" {
			return "", errors.New("invalid domain: empty label (leading or consecutive dot)")
		}
		if len(label) > 63 {
			return "", errors.New("invalid domain: each label is limited to 63 characters")
		}
		if !isDomainChar(label[0]) || !isDomainChar(label[len(label)-1]) {
			return "", errors.New("invalid domain: each label must start and end with a letter or digit")
		}
	}
	return d, nil
}

// isDomainChar reports whether b is a lowercase letter or a digit — the only
// characters allowed at a label's first and last position.
func isDomainChar(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
}
