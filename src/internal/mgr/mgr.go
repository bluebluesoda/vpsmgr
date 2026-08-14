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

// nameRe follows Incus instance-name rules (lowercase letters/digits/hyphens,
// max 63, no trailing hyphen — Incus rejects "abc-") plus a leading-letter
// requirement so a username can never start with a digit.
var nameRe = regexp.MustCompile(`^[a-z]([a-z0-9-]*[a-z0-9])?$`)

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

	// throttled tracks which containers currently carry a traffic throttle
	// (name -> on), so EnforceTrafficLimits only touches Incus on state changes.
	// limitMu guards it: the 60s sampler writes, the panel reads via
	// IsThrottled. Re-applying a limit after a panel restart is harmless (Incus
	// applies NIC limits live, no container restart).
	limitMu   sync.Mutex
	throttled map[string]bool

	// sampleMu serializes traffic sampling. Incus counters are read and the DB
	// delta applied in one critical section so a slower, out-of-order sampler
	// (the 60s goroutine vs a panel-triggered sample) can never write a lower
	// counter after a higher one was recorded — the SQL reset path would then
	// mistake the stale value for a container restart and double-count it.
	sampleMu sync.Mutex
}

func New(c *cfg.Config, d *db.DB) *Manager {
	return &Manager{cfg: c, db: d, lx: lx.New(c.Incus.Socket), fw: fw.New(c), tfx: tfx.New(c)}
}

func ValidateName(name string) error {
	if len(name) > 63 || !nameRe.MatchString(name) {
		return errors.New("invalid name: must start with a letter, then lowercase letters/digits/hyphens, max 63, no trailing hyphen")
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
// Incus, or -1 if it cannot be determined.
func (m *Manager) PoolUsage() (float64, error) {
	total, used, err := m.lx.PoolResources(m.cfg.Incus.Pool)
	if err != nil {
		return -1, nil
	}
	if total <= 0 {
		return -1, nil
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
	// TrafficGB is the monthly traffic quota in GiB (0 = unlimited).
	TrafficGB int
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
	idx, err := m.db.NextFreeIdx()
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
	ipv6, _ := m.IPv6Addr(name)
	block, _ := m.IPv6Block(name)
	if err := m.checkIPv6BlockCollision(name, block); err != nil {
		return nil, err
	}
	// Defend against orphan containers: a crashed create (or an out-of-band
	// `incus` instance) could already hold this name or the IP NextFreeIdx just
	// gave us. Refuse rather than create a bridge IP conflict.
	if err := m.checkIncusConflict(name, ip); err != nil {
		return nil, err
	}
	blockStr := ""
	if block != nil {
		blockStr = block.String()
	}
	// Make sure the image is present locally (a remote-qualified fallback like
	// "images:debian/13" is pulled first; the API cannot auto-fetch it inside
	// the create call the way the old `incus launch` CLI did).
	if err := m.lx.EnsureImage(image); err != nil {
		return nil, fmt.Errorf("ensure image %s: %w", image, err)
	}
	if err := m.lx.Launch(m.cfg.Incus.Pool, m.cfg.Incus.Bridge, name, image, ip, ipv6, blockStr, opt.CPU, opt.MemMB, opt.DiskGB); err != nil {
		return nil, fmt.Errorf("launch container: %w", err)
	}
	// From here on any failure must roll the container and its host-side
	// plumbing back, so a half-created user cannot leak a container, an IPv6
	// route, nft rules or a dangling database record.
	var createdID int64
	cleanup := func() {
		m.UnwireIPv6(name)
		_ = m.lx.Delete(name)
		_ = m.fw.RemoveUser(name)
		_ = m.fw.Reload()
		if createdID != 0 {
			_ = m.db.DeleteUser(createdID)
		}
	}
	if err := m.Provision(name, image, pass); err != nil {
		cleanup()
		return nil, fmt.Errorf("provision container: %w", err)
	}
	// IPv6 pass-through: attach the /128 route + proxy_ndp entry for the
	// container's SLAAC global address (no-op when IPv6 is disabled).
	if err := m.WireIPv6(name); err != nil {
		cleanup()
		return nil, fmt.Errorf("wire ipv6: %w", err)
	}
	// Host-routed peer IPv6 (no L2 discovery / MITM between containers).
	if err := m.ConfigureContainerIPv6(name); err != nil {
		cleanup()
		return nil, fmt.Errorf("config container ipv6: %w", err)
	}
	u, err := m.db.CreateUserFull(name, hash, ip, idx, sshPort, startPort, opt.CPU, opt.MemMB, opt.DiskGB, opt.TrafficGB)
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

// sampleUsage reads CPU/memory usage twice ~1s apart to derive CPU percentage.
func (m *Manager) sampleUsage() (map[string]lx.Usage, error) {
	u1, err := m.lx.UsageMap()
	if err != nil {
		return nil, err
	}
	time.Sleep(time.Second)
	u2, err := m.lx.UsageMap()
	if err != nil {
		return nil, err
	}
	out := make(map[string]lx.Usage)
	for name, v := range u2 {
		if prev, ok := u1[name]; ok {
			v.CPUUsage -= prev.CPUUsage
			out[name] = v
		}
	}
	return out, nil
}

func (m *Manager) decorateUsage(r *Result, use map[string]lx.Usage) {
	if u, ok := use[r.User.Name]; ok && r.User.CPU > 0 {
		pct := float64(u.CPUUsage) / 1e9 / (float64(r.User.CPU) / 10) * 100
		if pct < 0 {
			pct = 0
		}
		r.CPUUse = fmt.Sprintf("%.0f%%", pct)
		r.MemUse = fmt.Sprintf("%d MiB", u.MemUsage/(1<<20))
		return
	}
	r.CPUUse = "-"
	r.MemUse = "-"
}

func (m *Manager) ResultFor(u *db.User, pass string) *Result {
	st, _ := m.lx.State(u.Name)
	domains, _ := m.db.ListDomains(u.ID)
	ds := make([]string, len(domains))
	for i, d := range domains {
		ds[i] = d.Domain
	}
	up, down := m.TrafficFor(u.ID)
	ipv6, _ := m.IPv6Addr(u.Name)
	block := ""
	if b, _ := m.IPv6Block(u.Name); b != nil {
		block = b.String()
	}
	return &Result{User: u, Password: pass, PublicIP: m.cfg.DisplayIP(),
		State: st, Domains: ds, PortsPerUser: cfg.PortsPerUser,
		UpGB: FormatGB(up), DownGB: FormatGB(down), IPv6: ipv6, IPv6Block: block,
		V4Forward: m.cfg.Net.V4Forward}
}

func (m *Manager) Del(name string) error {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	if err := ValidateName(name); err != nil {
		return err
	}
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return err
	}
	// If the container cannot actually be removed, keep the DB record and let
	// the admin retry. Deleting the row anyway would orphan the container and
	// let NextFreeIdx reuse its IP/ports for a new user — a bridge IP conflict.
	if err := m.lx.Delete(name); err != nil {
		return fmt.Errorf("delete container: %w", err)
	}
	m.UnwireIPv6(name)
	// Capture the domains BEFORE the user row is deleted (DeleteUser cascades
	// them away) so their per-domain traefik files can be removed after.
	domains, _ := m.db.ListDomains(u.ID)
	// The remaining cleanup is best-effort: leftover nft rules / traefik
	// config without a container are harmless and re-runnable on retry.
	if err := m.fw.RemoveUser(name); err != nil {
		fmt.Printf("  ! warn: remove nft rules: %v\n", err)
	}
	if err := m.fw.Reload(); err != nil {
		fmt.Printf("  ! warn: reload nft: %v\n", err)
	}
	if err := m.db.DeleteUser(u.ID); err != nil {
		return err
	}
	if err := m.db.DeleteSessionsForUser(u.ID); err != nil {
		return err
	}
	for _, d := range domains {
		if err := m.tfx.RemoveDomain(d.Domain); err != nil {
			fmt.Printf("  ! warn: remove traefik config for %s: %v\n", d.Domain, err)
		}
	}
	m.limitMu.Lock()
	delete(m.throttled, name)
	m.limitMu.Unlock()
	return nil
}

// ApplyV4State enforces the current v4_forward policy: it rewrites (when on)
// or removes (when off) every user's DNAT rules, reloads the ruleset, and
// starts/stops the traefik service to match. Called by
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

// ApplyTraefikState starts/stops the traefik service to match v4_forward: with
// IPv4 inbound off, the domain proxy is not offered, so traefik is stopped and
// its BOOT AUTOSTART is disabled (systemctl disable --now) — it must not come
// back on the next reboot. Domain config files are KEPT, so re-enabling
// restores them; a full re-sync runs when enabling. systemctl errors are
// surfaced, not swallowed.
func (m *Manager) ApplyTraefikState() error {
	if m.cfg.Net.V4Forward {
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
	m.SampleTraffic() // best-effort freshness for the displayed totals
	use, _ := m.sampleUsage()
	out := make([]*Result, 0, len(users))
	for _, u := range users {
		r := m.ResultFor(u, "")
		m.decorateUsage(r, use)
		out = append(out, r)
	}
	return out, nil
}

func (m *Manager) Show(name string) (*Result, error) {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return nil, err
	}
	m.SampleTraffic() // best-effort freshness for the displayed totals
	r := m.ResultFor(u, "")
	use, _ := m.sampleUsage()
	m.decorateUsage(r, use)
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

// UpdateQuotasAndTraffic adjusts CPU/mem/disk and the monthly bandwidth quota
// in one call, so the admin panel's single submit cannot succeed halfway. The
// traffic quota is a DB-only write: it is applied first and rolled back if the
// Incus-side resource update fails. trafficGB < 0 leaves the traffic quota
// unchanged.
func (m *Manager) UpdateQuotasAndTraffic(name string, cpu, memMB, diskGB, trafficGB int) (*Result, error) {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return nil, err
	}
	if trafficGB >= 0 {
		if err := m.db.UpdateTrafficQuota(u.ID, trafficGB); err != nil {
			return nil, err
		}
	}
	res, err := m.UpdateQuotas(name, cpu, memMB, diskGB)
	if err != nil {
		if trafficGB >= 0 {
			// restore the previous traffic quota so the form is fully rolled back
			_ = m.db.UpdateTrafficQuota(u.ID, u.TrafficQuotaGB)
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
	if err := m.lx.Delete(u.Name); err != nil {
		return "", fmt.Errorf("delete container: %w", err)
	}
	// Default image (or empty): resolve to the configured alias / fallback and
	// pull the fallback if it is remote-qualified. A user-picked non-default
	// image must already exist locally — never auto-fetch a surprise image.
	isDefault := image == "" || image == m.cfg.Incus.Image
	if isDefault {
		image, err = m.imageName()
		if err != nil {
			return "", err
		}
		if err := m.lx.EnsureImage(image); err != nil {
			return "", fmt.Errorf("ensure image %s: %w", image, err)
		}
	} else if ok, _ := m.lx.ImageExists(image); !ok {
		return "", fmt.Errorf("image %s is not available on this host (run the image build script)", image)
	}
	ipv6, _ := m.IPv6Addr(u.Name)
	block, _ := m.IPv6Block(u.Name)
	blockStr := ""
	if block != nil {
		blockStr = block.String()
	}
	if err := m.lx.Launch(m.cfg.Incus.Pool, m.cfg.Incus.Bridge, u.Name, image, u.IP, ipv6, blockStr, u.CPU, u.MemMB, u.DiskGB); err != nil {
		return "", fmt.Errorf("recreate container: %w", err)
	}
	pass := pw.Generate(20)
	if err := m.Provision(u.Name, image, pass); err != nil {
		return "", fmt.Errorf("provision container: %w", err)
	}
	if err := m.WireIPv6(u.Name); err != nil {
		return "", fmt.Errorf("wire ipv6: %w", err)
	}
	if err := m.ConfigureContainerIPv6(u.Name); err != nil {
		return "", fmt.Errorf("config container ipv6: %w", err)
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
	return pass, nil
}

// AddDomain binds a domain to a user. DB and traefik YAML are kept in sync
// atomically: insert the DB row, write the domain's YAML file, and if the file
// write fails roll the DB row back so the two never disagree.
func (m *Manager) AddDomain(name, domain string, proxyProtocol bool) error {
	if !m.V4ForwardLive() {
		return errors.New("v4 forwarding is disabled (v4_forward: false) — domains are not available; re-enable with `vps config set net.v4_forward true`")
	}
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return err
	}
	domain, err = normalizeDomain(domain)
	if err != nil {
		return err
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

// EnsureBlockRoutes adds the deterministic /112 block (ipv6.routes) to every
// existing container's eth0, so an upgrade to the /112 scheme routes each
// container's whole block. Idempotent; a container without IPv6 (or not in
// Incus) is skipped. Restarts containers that needed the change.
func (m *Manager) EnsureBlockRoutes() error {
	if !m.cfg.IPv6Enabled() {
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

// domainRe allows only lowercase letters, digits, dots and hyphens, starting
// with an alphanumeric. The trailing letter check is applied separately.
var domainRe = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*$`)

// normalizeDomain validates and normalizes a domain for use in Traefik
// dynamic YAML. Strictly limited to [a-z0-9.-] and must end with a letter so
// it cannot break YAML syntax or inject extra Traefik config.
func normalizeDomain(d string) (string, error) {
	d = strings.TrimSpace(strings.ToLower(d))
	d = strings.TrimSuffix(d, ".")
	if d == "" {
		return "", errors.New("domain empty")
	}
	if len(d) > 253 {
		return "", errors.New("domain too long")
	}
	if !domainRe.MatchString(d) {
		return "", errors.New("invalid domain: only letters, digits, dots and hyphens allowed")
	}
	if last := d[len(d)-1]; last < 'a' || last > 'z' {
		return "", errors.New("invalid domain: must end with a letter")
	}
	return d, nil
}
