package cfg

import (
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"vpsmgr/internal/pw"
)

const (
	DefaultDataDir    = "/etc/vpsmgr"
	DefaultNftDir     = "/etc/vpsmgr/nftables.d"
	DefaultNftMain    = "/etc/vpsmgr/nftables.conf"
	DefaultTraefikDir = "/etc/traefik/dynamic"
	DefaultDB         = "/etc/vpsmgr/vpsmgr.db"
	DefaultListen     = ":8443"
	DefaultSubnet     = "10.115.0.0/24"
	DefaultGateway    = "10.115.0.1"
	DefaultBridge     = "incusbr0"
	DefaultPool       = "vpsmgr"
	DefaultImage      = "vpsmgr/debian-sshd"
	DefaultImageFB    = "images:debian/13"
	// DefaultSocket is the Incus daemon's Unix socket; the REST client
	// talks to it directly (no `incus` process spawn per call).
	DefaultSocket = "/var/lib/incus/unix.socket"

	// Panel listen port: a FRESH install picks a random free port in
	// PanelPortMin..PanelPortMax (written to panel.listen). DefaultListen
	// (8443) is only a code-level fallback, never the fresh-install default.
	PanelPortMin     = 2000
	PanelPortMax     = 9999
	DefaultPanelPort = 8443

	// Port scheme (fixed at install, immutable): each container gets one
	// random SSH port from SSHPortBase..SSHPortBase+SSHPortCount-1, plus a
	// whole-hundred block of PortsPerUser user ports starting at
	// UserPortBase+(idx-1)*PortsPerUser. 10000..29999 holds exactly
	// 200 blocks x 100 ports = 20000, so the range is fully packed.
	SSHPortBase  = 30000
	SSHPortCount = 2000
	UserPortBase = 10000
	PortsPerUser = 100
	MaxUsers     = 200

	// MaxInitScriptBytes caps a user's custom init script (run inside the
	// container after a reinstall). Bounds the DB row and the panel payload.
	MaxInitScriptBytes = 64 * 1024

	// Traffic throttle: when a user exceeds their monthly quota, both
	// directions are limited to ThrottleRate (an Incus NIC limit value, bit/s
	// with suffix). ThrottleDisplay is what the user panel shows.
	ThrottleRate    = "1Mbit"
	ThrottleDisplay = "1Mbps"
)

// GeneratedBanner is prepended to every file the panel generates, telling
// operators the file is managed and will be overwritten. '# ' is a valid
// comment in nftables, YAML, sysctl and systemd unit files alike.
const GeneratedBanner = "# Managed by vpsmgr — generated file, do not edit by hand.\n" +
	"# Changes are overwritten on the next write; use the panel / `vps` CLI instead.\n"

type Config struct {
	Panel PanelCfg `yaml:"panel"`
	Net   NetCfg   `yaml:"net"`
	Incus IncusCfg `yaml:"incus"`
}

type PanelCfg struct {
	Listen   string `yaml:"listen"`
	Cert     string `yaml:"cert"`
	Key      string `yaml:"key"`
	DB       string `yaml:"db"`
	PublicIP string `yaml:"public_ip"`
	// DisplayIP is a PURELY COSMETIC public address shown to users (panel URL,
	// SSH hints). On NAT-ing clouds (AWS/Alibaba) public_ip is a private NIC
	// address and this holds the publicly reachable one. Empty = fall back to
	// PublicIP. Never used by the firewall or routing.
	DisplayIP   string `yaml:"display_ip,omitempty"`
	SessionDays int    `yaml:"session_days"`
	// URLPath is the immutable secret prefix protecting the whole panel
	// (e.g. /Ab1_cdE-9x). Generated once on first install.
	URLPath string `yaml:"url_path"`
	// AdminPath is the secret prefix of the admin panel (e.g. /Xy-9ab_cdE),
	// a second random path generated at install. Requests that match neither
	// URLPath nor AdminPath get a bare, headerless 404.
	AdminPath string `yaml:"admin_url_path,omitempty"`
}

type NetCfg struct {
	Subnet  string `yaml:"subnet"`
	Gateway string `yaml:"gateway"`
	ExtIF   string `yaml:"ext_if"`
	// V4Forward controls IPv4 inbound forwarding to containers. When false
	// (only meaningful with IPv6 pass-through enabled) containers become
	// IPv6-only: no SSH DNAT, no user-port-block DNAT, and traefik (domains)
	// is disabled — containers still reach IPv4 out via the NAT4 masquerade.
	// Set once at install, changeable at runtime with `vps config set
	// net.v4_forward true|false`.
	// Deliberately NOT omitempty: false must round-trip through the config.
	V4Forward bool `yaml:"v4_forward"`
	// IPv6Subnet is the global prefix handed out to containers (e.g.
	// "2602:fada:6::/64", or a /80 slice the provider assigned the host).
	// Empty means IPv6 pass-through is disabled.
	// Containers get global addresses via SLAAC on incusbr0; the host proxies
	// their neighbor discovery. No NAT, no DB schema change: a container's
	// IPv6 is whatever Incus/SLAAC assigned, read live from `incus list`.
	IPv6Subnet string `yaml:"ipv6_subnet,omitempty"`
}

type IncusCfg struct {
	Image         string `yaml:"image"`
	ImageFallback string `yaml:"image_fallback"`
	Pool          string `yaml:"pool"`
	Bridge        string `yaml:"bridge"`
	// Socket is the path to the Incus daemon's Unix socket. Auto-detected
	// default matches the Debian package install
	// (`/var/lib/incus/unix.socket`).
	Socket string `yaml:"socket,omitempty"`
}

func Default() *Config {
	c := &Config{}
	c.Panel = PanelCfg{Listen: DefaultListen, Cert: DefaultDataDir + "/panel.crt", Key: DefaultDataDir + "/panel.key", DB: DefaultDB, SessionDays: 3}
	c.Net = NetCfg{Subnet: DefaultSubnet, Gateway: DefaultGateway, V4Forward: true}
	c.Incus = IncusCfg{Image: DefaultImage, ImageFallback: DefaultImageFB, Pool: DefaultPool, Bridge: DefaultBridge, Socket: DefaultSocket}
	return c
}

func Path() string {
	if p := os.Getenv("VPSMGR_CONFIG"); p != "" {
		return p
	}
	return DefaultDataDir + "/config.yaml"
}

func (c *Config) DataDir() string { return DefaultDataDir }
func (c *Config) NftDir() string  { return DefaultNftDir }
func (c *Config) NftMain() string { return DefaultNftMain }

// TraefikDir is where per-domain dynamic files are written. VPSMGR_TRAEFIK_DIR
// overrides it (used by tests to keep writes out of /etc/traefik).
func (c *Config) TraefikDir() string {
	if p := os.Getenv("VPSMGR_TRAEFIK_DIR"); p != "" {
		return p
	}
	return DefaultTraefikDir
}

// SubnetIP returns the IP portion of the subnet CIDR.
func (c *Config) SubnetIP() string {
	ip, _, err := net.ParseCIDR(c.Net.Subnet)
	if err != nil {
		return ""
	}
	return ip.String()
}

func Load() (*Config, error) {
	b, err := os.ReadFile(Path())
	if err != nil {
		return nil, err
	}
	c := Default()
	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, err
	}
	if err := c.FillAuto(); err != nil {
		return nil, err
	}
	return c, nil
}

func Save(c *Config) error {
	p := Path()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, MustYAML(c), 0o600)
}

func MustYAML(c *Config) []byte {
	b, err := yaml.Marshal(c)
	if err != nil {
		panic(err)
	}
	return b
}

// FillAuto fills every AUTO-detected field that is still empty: ext_if,
// public_ip and display_ip. The two secret panel paths are NOT generated here —
// an empty url_path/admin_url_path means that panel is DISABLED. Paths are only
// generated by EnsurePaths on a fresh install.
func (c *Config) FillAuto() error {
	if c.Net.ExtIF == "" {
		c.Net.ExtIF = DetectExtIF()
	}
	if c.Panel.PublicIP == "" {
		c.Panel.PublicIP = DetectPublicIP(c.Net.ExtIF)
	}
	// Display IP: on NAT-ing clouds public_ip is a private NIC address that is
	// unreachable from outside. When that happens, fetch a public IPv4 from a
	// stable echo service purely for display. Graceful: if the fetch fails
	// display falls back to public_ip.
	if c.Panel.DisplayIP == "" && isPrivateIPv4(c.Panel.PublicIP) {
		c.Panel.DisplayIP = httpGet("https://ipv4.ip.sb", "GET", nil, 3)
	}
	// VPSMGR_IPV6_SUBNET lets the installer inject the /64 prefix at first
	// install (it overrides whatever is in the config file).
	if v := os.Getenv("VPSMGR_IPV6_SUBNET"); v != "" {
		c.Net.IPv6Subnet = v
	}
	// VPSMGR_IPV4_SUBNET carries the container subnet chosen at install time
	// (e.g. 10.115.0.0/24); the gateway is derived from it.
	if v := os.Getenv("VPSMGR_IPV4_SUBNET"); v != "" {
		c.Net.Subnet = v
		if g := GatewayFromSubnet(v); g != "" {
			c.Net.Gateway = g
		}
	}
	// VPSMGR_V4_FORWARD (1/0/true/false) carries the IPv4 inbound policy chosen
	// at install time; on adoption the config value is re-exported by the ask
	// script, so this just mirrors it.
	if v := os.Getenv("VPSMGR_V4_FORWARD"); v != "" {
		c.Net.V4Forward = v == "1" || strings.EqualFold(v, "true")
	}
	return nil
}

// EnsurePaths generates both secret panel paths when NEITHER is configured.
// This is the fresh-install case: a user panel without admin (or vice versa)
// is a deliberate choice to disable one side, so it is respected. When both
// are empty the whole panel service is off — only `vps install` calls this,
// and only when creating a brand-new config.
func (c *Config) EnsurePaths() {
	if c.Panel.URLPath == "" && c.Panel.AdminPath == "" {
		c.Panel.URLPath = pw.URLSafe(10)
		c.Panel.AdminPath = pw.URLSafe(12)
	}
}

// DisplayIP returns the address shown to users (panel URL, SSH hints,
// "vps add/show" output): the configured display_ip when set, otherwise
// public_ip. Purely cosmetic — the firewall and routing keep using PublicIP.
func (c *Config) DisplayIP() string {
	if c.Panel.DisplayIP != "" {
		return c.Panel.DisplayIP
	}
	return c.Panel.PublicIP
}

// PanelPort returns the TCP port the panel listens on, parsed from
// panel.listen (e.g. ":8443" or "127.0.0.1:8443"). Falls back to the
// code-level default when the value is empty or unparseable.
func (c *Config) PanelPort() int {
	if _, p, err := net.SplitHostPort(c.Panel.Listen); err == nil {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			return n
		}
	}
	return DefaultPanelPort
}

// PanelURL renders the user/admin panel URL from display_ip and the actual
// listen port (the port is NOT hardcoded 8443 — a fresh install picks a random
// one in 2000-9999).
func (c *Config) PanelURL(prefix string) string {
	return "https://" + c.DisplayIP() + ":" + strconv.Itoa(c.PanelPort()) + prefix
}

// GatewayFromSubnet returns the .1 gateway of an IPv4 CIDR, or "" when the
// subnet is not a parseable IPv4 network.
func GatewayFromSubnet(subnet string) string {
	_, ipnet, err := net.ParseCIDR(subnet)
	if err != nil {
		return ""
	}
	gw := ipnet.IP.To4()
	if gw == nil {
		return ""
	}
	gw[3] = 1
	return gw.String()
}

// RandomPanelPort returns a random port in PanelPortMin..PanelPortMax that is
// currently free to bind (tries 64 random picks, then the lowest free one).
// It is only a best-effort check at install time — nothing else on a fresh
// host binds the range, so a collision afterwards is not expected.
func RandomPanelPort() (int, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(PanelPortMax-PanelPortMin+1))
	if err == nil {
		for i := 0; i < 64; i++ {
			p := PanelPortMin + int(n.Int64())
			if panelPortFree(p) {
				return p, nil
			}
			n, err = rand.Int(rand.Reader, big.NewInt(PanelPortMax-PanelPortMin+1))
			if err != nil {
				break
			}
		}
	}
	for p := PanelPortMin; p <= PanelPortMax; p++ {
		if panelPortFree(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free panel port in %d-%d", PanelPortMin, PanelPortMax)
}

func panelPortFree(p int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", p))
	if err != nil {
		return false
	}
	ln.Close()
	return true
}

// ValidatePaths checks the secret panel paths that are ENABLED (non-empty):
// each must be at least 10 chars, and the two must not collide. An empty path
// means that panel is intentionally disabled. When both are empty the whole
// panel service is off (cmdServe does not listen).
func (c *Config) ValidatePaths() error {
	if c.Panel.URLPath == "" && c.Panel.AdminPath == "" {
		return nil
	}
	if c.Panel.URLPath != "" && len(c.Panel.URLPath) < 10 {
		return fmt.Errorf("panel url_path too short (%d chars, want >= 10)", len(c.Panel.URLPath))
	}
	if c.Panel.AdminPath != "" && len(c.Panel.AdminPath) < 10 {
		return fmt.Errorf("panel admin_url_path too short (%d chars, want >= 10)", len(c.Panel.AdminPath))
	}
	if c.Panel.URLPath != "" && c.Panel.AdminPath != "" && c.Panel.URLPath == c.Panel.AdminPath {
		return fmt.Errorf("panel url_path and admin_url_path must differ")
	}
	return nil
}

// isPrivateIPv4 reports whether s is a non-public IPv4: RFC1918, CGNAT
// (100.64.0.0/10), link-local or loopback.
func isPrivateIPv4(s string) bool {
	ip := net.ParseIP(s)
	if ip == nil || ip.To4() == nil {
		return false
	}
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return true
	}
	v4 := ip.To4()
	return v4[0] == 100 && v4[1]&0xc0 == 64
}

// IPv6Enabled reports whether IPv6 pass-through is configured.
func (c *Config) IPv6Enabled() bool { return c.Net.IPv6Subnet != "" }

// IPv6Network parses and validates the configured IPv6 prefix. It must be a
// global (non-ULA, non-link-local) CIDR — /64 or shorter (e.g. /56), or longer
// up to /80 when the provider hands the host a /80 slice. The prefix length is
// REQUIRED; a bare address is rejected rather than silently assumed to be /64
// (a /80 slice would then get addresses outside the routed prefix).
func (c *Config) IPv6Network() (*net.IPNet, error) {
	s := c.Net.IPv6Subnet
	if s == "" {
		return nil, fmt.Errorf("ipv6_subnet not configured")
	}
	if !strings.Contains(s, "/") {
		return nil, fmt.Errorf("invalid ipv6_subnet %q: prefix length required (e.g. /64 or /80)", s)
	}
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		return nil, fmt.Errorf("invalid ipv6_subnet %q: %w", s, err)
	}
	if n.IP.To4() != nil {
		return nil, fmt.Errorf("invalid ipv6_subnet %q: not an IPv6 prefix", s)
	}
	if n.IP.IsPrivate() || n.IP.IsLinkLocalUnicast() || n.IP.IsLoopback() || n.IP.IsUnspecified() {
		return nil, fmt.Errorf("invalid ipv6_subnet %q: must be a global (public) prefix", s)
	}
	ones, _ := n.Mask.Size()
	// The deterministic per-container address uses the low 48 host bits (a
	// 32-bit username hash + a fixed 0001 last block), so the prefix needs at
	// least that many host bits: any prefix /80 or shorter works.
	if ones > 80 {
		return nil, fmt.Errorf("invalid ipv6_subnet %q: prefix must be /80 or shorter (got /%d)", s, ones)
	}
	return n, nil
}

func shCmd(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// DetectExtIF returns the name of the interface used for the default route.
func DetectExtIF() string {
	if s := shCmd("sh", "-c", "ip route show default | awk '{print $5; exit}'"); s != "" {
		return s
	}
	if s := shCmd("sh", "-c", "ip -o link show up | grep -v -E 'lo|incusbr|virbr|docker' | awk -F': ' '{print $2; exit}'"); s != "" {
		return s
	}
	return "eth0"
}

// DetectPublicIP returns the machine's own IPv4 on the external interface
// (falling back to hostname -I). This is the address the firewall and routing
// use. On clouds that NAT (AWS EC2, Alibaba ECS) it is a PRIVATE address —
// the publicly reachable one is handled separately via DisplayIP.
func DetectPublicIP(extIF string) string {
	if extIF != "" {
		ip, err := firstIPv4(extIF)
		if err == nil && ip != "" {
			return ip
		}
	}
	if s := shCmd("sh", "-c", "hostname -I | awk '{print $1}'"); s != "" {
		return s
	}
	return "127.0.0.1"
}

// httpGet issues a request and returns the trimmed body (up to 64 bytes), or
// "" on any failure / non-200.
func httpGet(url, method string, headers map[string]string, timeoutSec int) string {
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		return ""
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := &http.Client{Timeout: time.Duration(timeoutSec) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func firstIPv4(iface string) (string, error) {
	ifi, err := net.InterfaceByName(iface)
	if err != nil {
		return "", err
	}
	addrs, err := ifi.Addrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		}
		if ip != nil && ip.To4() != nil && !ip.IsLoopback() {
			return ip.String(), nil
		}
	}
	return "", fmt.Errorf("no ipv4 on %s", iface)
}
