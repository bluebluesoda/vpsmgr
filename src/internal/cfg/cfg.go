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
	"sort"
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

	// DefaultSwapRatio is the swap granted to each container as a multiple
	// of its memory limit (limits.memory.swap = limits.memory * ratio).
	// Incus 7 on cgroup v2 sets memory.swap.max to 0 unless limits.memory.swap
	// is given a byte amount, so without this every container gets zero swap.
	// 0.5 means a 1 GiB container may use up to 512 MiB of host swap — the
	// host swap pool is shared by all containers, so a ratio < 1 avoids
	// over-subscribing it (see docs/configuration.md).
	DefaultSwapRatio = 0.5

	// Panel listen port: a FRESH install picks a random free port in
	// PanelPortMin..PanelPortMax (written to panel.listen). DefaultListen
	// (8443) is only a code-level fallback, never the fresh-install default.
	PanelPortMin     = 2000
	PanelPortMax     = 9999
	DefaultPanelPort = 8443

	// Port scheme (fixed at install, immutable): each container gets one
	// random SSH port from SSHPortBase..SSHPortBase+SSHPortCount-1, plus a
	// whole-hundred block of PortsPerUser user ports. The block a NEW
	// container takes is drawn from the configured net.user_ports ranges
	// (default 10000-29999 = 200 blocks x 100 ports = 20000, fully packed).
	// Existing containers keep their start_port regardless of later range
	// edits — only new users are affected.
	SSHPortBase  = 30000
	SSHPortCount = 2000
	UserPortBase = 10000
	PortsPerUser = 100
	MaxUsers     = 200 // absolute container ceiling (IPv4 /24 host bits); port blocks are the real limiter

	// User port domain. A container's block is always a whole hundred inside
	// UserPortBase..UserPortMax; net.user_ports may name any sub-range(s) and
	// is auto-aligned to whole hundreds (low end rounds up, high end rounds
	// down then +99 so the last block ends in ...99, never ...00).
	UserPortMax      = UserPortBase + MaxUsers*PortsPerUser - 1 // 29999
	DefaultUserPorts = "10000-29999"

	// Legacy slot range bounds, kept only to migrate pre-user_ports configs
	// (slot_range "lo-hi" of v4 last octets). See LegacySlotRangeToUserPorts.
	DefaultSlotMin = 2
	DefaultSlotMax = 201

	// MaxInitScriptBytes caps a user's custom init script (run inside the
	// container after a reinstall). Bounds the DB row and the panel payload.
	MaxInitScriptBytes = 64 * 1024

	// MaxNotesPlaintextBytes caps the decrypted sticky-notes JSON, checked on
	// the client before encryption. MaxNotesBlobBytes caps the stored
	// (base64) encrypted envelope on the server, leaving room for the GCM tag,
	// salt and IV plus base64 expansion.
	MaxNotesPlaintextBytes = 256 * 1024
	MaxNotesBlobBytes      = 700 * 1024

	// Bandwidth throttle: when a user exceeds their monthly quota, both
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
	Panel     PanelCfg     `yaml:"panel"`
	Net       NetCfg       `yaml:"net"`
	Incus     IncusCfg     `yaml:"incus"`
	Snapshots SnapshotsCfg `yaml:"snapshots"`
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
	// Traefik controls the optional domain reverse proxy independently of
	// IPv4 forwarding. When false, Traefik is stopped and not enabled at boot;
	// existing domain records are retained but new domains cannot be added.
	Traefik bool `yaml:"traefik"`
	// UserPorts is the comma-separated set of inclusive user-port ranges a
	// NEW container's whole-hundred block may be drawn from (e.g.
	// "10000-29999", or "10000-20000, 25000-30000" for discontiguous spans).
	// Values are auto-aligned to whole hundreds and merged; only-new-users
	// effect: existing containers never change their ports. The default is
	// the full 10000-29999. Absent in an older config (carrying only the
	// legacy slot_range) it is migrated on Load.
	UserPorts string `yaml:"user_ports"`
	// SlotRange is the LEGACY pre-user_ports field (inclusive v4 last octet
	// range, "lo-hi"). Read-only for migration: when user_ports is empty a
	// set slot_range is converted to user_ports and the config rewritten. It
	// is no longer editable — the registry entry was removed.
	SlotRange string `yaml:"slot_range,omitempty"`
	// IPv6Subnet is the global prefix handed out to containers (e.g.
	// "2602:fada:6::/64", or a /80 slice the provider assigned the host).
	// Empty means IPv6 pass-through is disabled.
	// Containers get global addresses via SLAAC on incusbr0; the host proxies
	// their neighbor discovery. No NAT, no DB schema change: a container's
	// IPv6 is whatever Incus/SLAAC assigned, read live from `incus list`.
	IPv6Subnet string `yaml:"ipv6_subnet,omitempty"`
	// IPv6Mode is how containers get their global IPv6: "none" (disabled),
	// "prefix" (deterministic /112 blocks derived from the configured prefix)
	// or "pool" (one address picked from IPv6Pool per container, stored in the
	// DB). Fixed at install — switching modes would renumber every container.
	IPv6Mode string `yaml:"ipv6_mode,omitempty"`
	// IPv6Pool is the list of global /128 addresses a container can be
	// assigned in pool mode (typically the addresses the provider lets the
	// host bind individually). A user keeps its address until deleted.
	IPv6Pool []string `yaml:"ipv6_pool,omitempty"`
}

// IPv6 mode values.
const (
	IPv6ModeNone   = "none"
	IPv6ModePrefix = "prefix"
	IPv6ModePool   = "pool"
)

// IPv6ModeEffective returns the effective IPv6 mode, normalizing an empty
// mode string to a value consistent with the configured fields:
//   - ipv6_pool set -> pool
//   - ipv6_subnet set -> prefix
//   - otherwise -> none
//
// The installer writes the explicit mode, but an older config (or a hand edit)
// may leave it empty — normalize rather than refuse to run.
func (c *Config) IPv6ModeEffective() string {
	switch c.Net.IPv6Mode {
	case IPv6ModePool:
		return IPv6ModePool
	case IPv6ModePrefix:
		return IPv6ModePrefix
	case IPv6ModeNone:
		return IPv6ModeNone
	}
	if len(c.Net.IPv6Pool) > 0 {
		return IPv6ModePool
	}
	if c.Net.IPv6Subnet != "" {
		return IPv6ModePrefix
	}
	return IPv6ModeNone
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
	// SwapRatio is the swap allowed to each new container as a multiple of
	// its memory limit (limits.memory.swap = limits.memory * SwapRatio).
	// 0 disables container swap entirely; see DefaultSwapRatio.
	// Deliberately NOT omitempty: 0 must round-trip through the config.
	SwapRatio float64 `yaml:"swap_ratio"`
}

// SnapshotsCfg controls per-container user snapshots.
type SnapshotsCfg struct {
	// Limit caps how many snapshots a user may keep per container. The default
	// of 1 gives a light safety net; a snapshot is disk-only (no memory) and
	// lives under the container in Incus, so it is deleted together with the
	// container on reinstall/delete. 0 disables snapshots: users cannot create
	// new ones, but existing snapshots are left untouched. Negative is treated
	// as 0 (the config validator rejects negatives anyway).
	Limit int `yaml:"limit"`
}

func Default() *Config {
	c := &Config{}
	c.Panel = PanelCfg{Listen: DefaultListen, Cert: DefaultDataDir + "/panel.crt", Key: DefaultDataDir + "/panel.key", DB: DefaultDB, SessionDays: 3}
	c.Net = NetCfg{Subnet: DefaultSubnet, Gateway: DefaultGateway, V4Forward: true, Traefik: true, UserPorts: DefaultUserPorts}
	c.Incus = IncusCfg{Image: DefaultImage, ImageFallback: DefaultImageFB, Pool: DefaultPool, Bridge: DefaultBridge, Socket: DefaultSocket, SwapRatio: DefaultSwapRatio}
	c.Snapshots = SnapshotsCfg{Limit: 1}
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

// PortRange is an inclusive, whole-hundred-aligned user-port range. Lo and Hi
// are both within [UserPortBase, UserPortMax]; Lo%100==0 and Hi%100==99, so the
// range always spans a whole number of 100-port blocks.
type PortRange struct {
	Lo, Hi int
}

// ParseSlotRange parses a LEGACY slot range "<lo>-<hi>" into inclusive
// v4-last-octet bounds. Rules (strict): exactly one '-' separator, both ends
// non-empty integers, each within [DefaultSlotMin, DefaultSlotMax] (2..201),
// and lo <= hi. Kept for migrating pre-user_ports configs; no longer exposed
// as an editable config key.
func ParseSlotRange(s string) (lo, hi int, err error) {
	s = strings.TrimSpace(s)
	parts := strings.Split(s, "-")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return 0, 0, fmt.Errorf("invalid slot range %q: must be \"<lo>-<hi>\" (e.g. 2-201)", s)
	}
	a, err1 := strconv.Atoi(parts[0])
	b, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil {
		return 0, 0, fmt.Errorf("invalid slot range %q: both ends must be integers", s)
	}
	if a < DefaultSlotMin || a > DefaultSlotMax || b < DefaultSlotMin || b > DefaultSlotMax {
		return 0, 0, fmt.Errorf("invalid slot range %q: each end must be within %d..%d (the default range)", s, DefaultSlotMin, DefaultSlotMax)
	}
	if a > b {
		return 0, 0, fmt.Errorf("invalid slot range %q: lo (%d) must be <= hi (%d)", s, a, b)
	}
	return a, b, nil
}

// LegacySlotRangeToUserPorts converts a legacy slot_range ("lo-hi", inclusive
// v4 last octets) into the equivalent user-port range string. Slot idx
// (= octet-1) owned [UserPortBase+(idx-1)*PortsPerUser, +99], so lo octet maps
// to port 10000+(lo-2)*100 and hi octet to 10000+(hi-2)*100+99. The result is
// already whole-hundred aligned.
func LegacySlotRangeToUserPorts(slotRange string) (string, error) {
	lo, hi, err := ParseSlotRange(slotRange)
	if err != nil {
		return "", err
	}
	plo := UserPortBase + (lo-DefaultSlotMin)*PortsPerUser
	phi := UserPortBase + (hi-DefaultSlotMin)*PortsPerUser + PortsPerUser - 1
	return fmt.Sprintf("%d-%d", plo, phi), nil
}

// ParseUserPorts parses a comma-separated list of inclusive port ranges (each
// "<lo>-<hi>") into normalized, merged, whole-hundred-aligned ranges. Rules:
//   - Lo/hi may extend outside [UserPortBase, UserPortMax] — only the overlap
//     with the usable domain counts (a range fully outside contributes nothing).
//   - Each range is aligned inward to whole hundreds: the low end rounds UP to
//     a block start, the high end rounds DOWN to a block start then extends to
//     +99 (so the effective range always ends in ...99, never ...00). A range
//     narrower than one block is dropped.
//   - Overlapping/adjacent ranges are merged, so capacity is never double-counted.
//   - At least one usable 100-port block must remain, or the value is rejected
//     (a range that cannot host even one container is an error).
func ParseUserPorts(s string) ([]PortRange, error) {
	var out []PortRange
	for _, tok := range strings.Split(s, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		parts := strings.Split(tok, "-")
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			return nil, fmt.Errorf("invalid user-port range %q: must be \"<lo>-<hi>\" (e.g. 10000-29999)", tok)
		}
		a, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		b, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("invalid user-port range %q: both ends must be integers", tok)
		}
		if a > b {
			return nil, fmt.Errorf("invalid user-port range %q: lo (%d) must be <= hi (%d)", tok, a, b)
		}
		// Clamp to the usable domain; a range with no overlap contributes 0.
		if a < UserPortBase {
			a = UserPortBase
		}
		if b > UserPortMax {
			b = UserPortMax
		}
		if a > b {
			continue
		}
		// Align inward: lo up to a block start, hi down to a block start then +99.
		lo := ((a + PortsPerUser - 1) / PortsPerUser) * PortsPerUser
		hi := (b/PortsPerUser)*PortsPerUser + PortsPerUser - 1
		if lo > hi {
			continue // narrower than one block
		}
		out = append(out, PortRange{lo, hi})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no usable user-port range %q: must overlap %d-%d with at least one whole 100-port block", s, UserPortBase, UserPortMax)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Lo < out[j].Lo })
	merged := out[:1]
	for _, r := range out[1:] {
		last := &merged[len(merged)-1]
		if r.Lo <= last.Hi+1 { // overlap or adjacent → merge
			if r.Hi > last.Hi {
				last.Hi = r.Hi
			}
		} else {
			merged = append(merged, r)
		}
	}
	return merged, nil
}

// CanonicalUserPorts renders parsed ranges as their canonical comma-separated
// string form ("10000-20099, 25000-29999"). Returns the raw input unchanged if
// it does not parse.
func CanonicalUserPorts(s string) string {
	rs, err := ParseUserPorts(s)
	if err != nil {
		return strings.TrimSpace(s)
	}
	parts := make([]string, len(rs))
	for i, r := range rs {
		parts[i] = fmt.Sprintf("%d-%d", r.Lo, r.Hi)
	}
	return strings.Join(parts, ", ")
}

// UserPortRanges returns the configured effective user-port ranges, falling
// back to the default full range if the configured value does not parse (a
// validated config never hits this; guard against hand-edits).
func (c *Config) UserPortRanges() []PortRange {
	rs, err := ParseUserPorts(c.Net.UserPorts)
	if err != nil {
		rs, _ = ParseUserPorts(DefaultUserPorts)
	}
	return rs
}

// UserPortCount returns how many whole-hundred port blocks the configured
// ranges allow (== the container capacity a NEW user may still take; existing
// users outside the ranges keep their blocks and are excluded from the
// free-list window).
func (c *Config) UserPortCount() int {
	n := 0
	for _, r := range c.UserPortRanges() {
		n += (r.Hi - r.Lo + 1) / PortsPerUser
	}
	return n
}

// UserPortBlockStarts returns every whole-hundred block start inside the
// configured ranges, for the port-block allocator.
func (c *Config) UserPortBlockStarts() []int {
	var starts []int
	for _, r := range c.UserPortRanges() {
		for s := r.Lo; s <= r.Hi; s += PortsPerUser {
			starts = append(starts, s)
		}
	}
	return starts
}

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
	// Legacy migration: a pre-user_ports config carries only slot_range. It
	// must run here (not in FillAuto) because Default() pre-fills user_ports
	// with the full range, making "unset" indistinguishable from "defaulted" —
	// detect the key's actual presence in the file instead.
	if !hasYAMLKey(b, "user_ports") && c.Net.SlotRange != "" {
		if ports, err := LegacySlotRangeToUserPorts(c.Net.SlotRange); err == nil {
			c.Net.UserPorts = ports
		}
	}
	if err := c.FillAuto(); err != nil {
		return nil, err
	}
	return c, nil
}

// hasYAMLKey reports whether the config file carries a `key:` line. Used to
// tell "key absent (older config)" from "key present with the default value".
func hasYAMLKey(b []byte, key string) bool {
	for _, line := range strings.Split(string(b), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, key+":") {
			return true
		}
	}
	return false
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
	// VPSMGR_IPV6_MODE carries the IPv6 mode chosen at install (none/prefix/pool).
	if v := os.Getenv("VPSMGR_IPV6_MODE"); v != "" {
		c.Net.IPv6Mode = v
	}
	// VPSMGR_IPV6_POOL carries the address pool chosen at install (pool mode):
	// comma- or newline-separated global IPv6 addresses. Validated below via
	// IPv6PoolValidated so a bad list fails the install loudly.
	if v := os.Getenv("VPSMGR_IPV6_POOL"); v != "" {
		var addrs []string
		for _, part := range strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == '\n' || r == ' ' }) {
			if part != "" {
				addrs = append(addrs, part)
			}
		}
		if _, err := (&Config{Net: NetCfg{IPv6Pool: addrs}}).IPv6PoolValidated(); err != nil {
			return err
		}
		c.Net.IPv6Pool = addrs
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
	// VPSMGR_TRAEFIK (1/0/true/false) forces the Traefik domain-proxy toggle at
	// install: 00-check.sh sets it to 0 when 80/443 is already taken so the
	// install continues with Traefik installed but DISABLED (net.traefik
	// false) instead of failing or fighting the occupant.
	if v := os.Getenv("VPSMGR_TRAEFIK"); v != "" {
		c.Net.Traefik = v == "1" || strings.EqualFold(v, "true")
	}
	// Legacy migration happens in Load (see hasYAMLKey), NOT here: Default() has
	// already pre-filled user_ports with the full range by the time FillAuto
	// runs, so "unset" is indistinguishable from "defaulted" at this point.
	// VPSMGR_USER_PORTS carries the container user-port ranges chosen at
	// install (e.g. "10000-29999" or "10000-20000, 25000-30000"); strictly
	// validated so a bad value fails the install loudly instead of silently
	// defaulting.
	if v := os.Getenv("VPSMGR_USER_PORTS"); v != "" {
		if _, err := ParseUserPorts(v); err != nil {
			return fmt.Errorf("VPSMGR_USER_PORTS %q: %w", v, err)
		}
		c.Net.UserPorts = CanonicalUserPorts(v)
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

// IPv6Enabled reports whether IPv6 pass-through is configured (either mode).
func (c *Config) IPv6Enabled() bool { return c.IPv6ModeEffective() != IPv6ModeNone }

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

// IPv6PoolValidated validates and normalizes the configured address pool.
// Every entry must be a single global (non-ULA, non-link-local) IPv6 address;
// an explicit /128 suffix is accepted and normalized away, any other prefix
// length is rejected. Duplicates are rejected. Returns the canonical
// (compressed, bare) form of each address. An empty pool is valid; nil is
// returned only when there is nothing to validate.
func (c *Config) IPv6PoolValidated() ([]string, error) {
	if len(c.Net.IPv6Pool) == 0 {
		return nil, nil
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(c.Net.IPv6Pool))
	for _, s := range c.Net.IPv6Pool {
		s = strings.TrimSpace(s)
		if strings.Contains(s, "/") {
			ip, n, err := net.ParseCIDR(s)
			if err != nil || ip.To4() != nil {
				return nil, fmt.Errorf("invalid ipv6_pool entry %q: not an IPv6 address", s)
			}
			if ones, _ := n.Mask.Size(); ones != 128 {
				return nil, fmt.Errorf("invalid ipv6_pool entry %q: only a bare address or /128 is allowed (got /%d)", s, ones)
			}
			s = ip.String()
		}
		ip := net.ParseIP(s)
		if ip == nil || ip.To4() != nil {
			return nil, fmt.Errorf("invalid ipv6_pool entry %q: not an IPv6 address", s)
		}
		if ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLoopback() || ip.IsUnspecified() {
			return nil, fmt.Errorf("invalid ipv6_pool entry %q: must be a global (public) address", s)
		}
		canon := ip.String()
		if seen[canon] {
			return nil, fmt.Errorf("invalid ipv6_pool entry %q: duplicate address", s)
		}
		seen[canon] = true
		out = append(out, canon)
	}
	return out, nil
}

func shCmd(name string, args ...string) string {
	out, err := exec.Command(name, args...).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// DetectExtIF returns the name of the interface used for the default route.
// Tries, in order: the IPv4 default route, the IPv6 default route (covers
// IPv6-only hosts and policy-routed boxes whose v4 default lives in a
// non-main table), then the first non-virtual UP interface.
func DetectExtIF() string {
	if s := shCmd("sh", "-c", "ip route show default | awk '{for(i=1;i<=NF;i++) if($i==\"dev\"){print $(i+1); exit}}'"); s != "" {
		return s
	}
	if s := shCmd("sh", "-c", "ip -6 route show default | awk '{for(i=1;i<=NF;i++) if($i==\"dev\"){print $(i+1); exit}}'"); s != "" {
		return s
	}
	if s := shCmd("sh", "-c", "ip -o link show up | grep -v -E 'lo|incusbr|virbr|docker|veth|warp|wg' | awk -F': ' '{print $2; exit}'"); s != "" {
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
