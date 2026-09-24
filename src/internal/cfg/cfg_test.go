package cfg

import (
	"os"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestIPv6Network(t *testing.T) {
	cases := []struct {
		subnet string
		want   string // canonical CIDR; "" = must be rejected
	}{
		{"2602:fada:6::/64", "2602:fada:6::/64"},
		{"2406:da14:1dd2:a807:753a::/80", "2406:da14:1dd2:a807:753a::/80"},
		{"2001:db8::/32", "2001:db8::/32"},
		// A bare address must be rejected, not silently assumed /64 — a /80
		// slice would then get addresses outside the routed prefix.
		{"2602:fada:6::", ""},
		{"2406:da14:1dd2:a807:753a::", ""},
		// Non-global prefixes and /96 (too few host bits) must be rejected.
		{"fe80::/64", ""},
		{"2406:da14:1dd2:a807:753a::/96", ""},
	}
	for _, c := range cases {
		cfg := Default()
		cfg.Net.IPv6Subnet = c.subnet
		n, err := cfg.IPv6Network()
		if c.want == "" {
			if err == nil {
				t.Errorf("IPv6Network(%q): expected error, got %s", c.subnet, n)
			}
			continue
		}
		if err != nil {
			t.Errorf("IPv6Network(%q): unexpected error: %v", c.subnet, err)
			continue
		}
		if got := n.String(); got != c.want {
			t.Errorf("IPv6Network(%q) = %s, want %s", c.subnet, got, c.want)
		}
	}
}

func TestValidatePaths(t *testing.T) {
	good := Default()
	good.Panel.URLPath = "UserSecRet99"
	good.Panel.AdminPath = "Adm1n-SecretX"
	if err := good.ValidatePaths(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	// Empty path = panel disabled: a single enabled panel or none at all is OK.
	onlyUser := Default()
	onlyUser.Panel.URLPath = "UserSecRet99"
	onlyUser.Panel.AdminPath = ""
	if err := onlyUser.ValidatePaths(); err != nil {
		t.Fatalf("user-only config rejected: %v", err)
	}
	onlyAdmin := Default()
	onlyAdmin.Panel.URLPath = ""
	onlyAdmin.Panel.AdminPath = "Adm1n-SecretX"
	if err := onlyAdmin.ValidatePaths(); err != nil {
		t.Fatalf("admin-only config rejected: %v", err)
	}
	bothOff := Default()
	bothOff.Panel.URLPath = ""
	bothOff.Panel.AdminPath = ""
	if err := bothOff.ValidatePaths(); err != nil {
		t.Fatalf("both-disabled config rejected: %v", err)
	}
	// Rejections: too short (enabled path) or the two paths colliding.
	cases := []struct {
		name      string
		user, adm string
	}{
		{"short user", "short", "Adm1n-SecretX"},
		{"short admin", "UserSecRet99", "short"},
		{"equal", "SameSecret", "SameSecret"},
	}
	for _, c := range cases {
		cfg := Default()
		cfg.Panel.URLPath = c.user
		cfg.Panel.AdminPath = c.adm
		if err := cfg.ValidatePaths(); err == nil {
			t.Errorf("ValidatePaths(%q,%q): expected error", c.user, c.adm)
		}
	}
}

func TestEnsurePaths(t *testing.T) {
	// Both empty -> fresh install: both paths generated (user 10, admin 12).
	cfg := Default()
	cfg.EnsurePaths()
	if len(cfg.Panel.URLPath) != 10 {
		t.Errorf("url_path len = %d, want 10", len(cfg.Panel.URLPath))
	}
	if len(cfg.Panel.AdminPath) != 12 {
		t.Errorf("admin_url_path len = %d, want 12", len(cfg.Panel.AdminPath))
	}
	if cfg.Panel.URLPath == cfg.Panel.AdminPath {
		t.Fatal("generated paths must differ")
	}
	for _, s := range []string{cfg.Panel.URLPath, cfg.Panel.AdminPath} {
		for _, r := range s {
			if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_", r) {
				t.Fatalf("path %q contains invalid char %q", s, r)
			}
		}
	}
	// One side empty is a deliberate disable: EnsurePaths must NOT touch it.
	userOnly := Default()
	userOnly.Panel.URLPath = "UserSecRet99"
	userOnly.EnsurePaths()
	if userOnly.Panel.URLPath != "UserSecRet99" || userOnly.Panel.AdminPath != "" {
		t.Fatalf("EnsurePaths changed a deliberate user-only config: %q / %q", userOnly.Panel.URLPath, userOnly.Panel.AdminPath)
	}
	adminOnly := Default()
	adminOnly.Panel.AdminPath = "Adm1n-SecretX"
	adminOnly.EnsurePaths()
	if adminOnly.Panel.URLPath != "" || adminOnly.Panel.AdminPath != "Adm1n-SecretX" {
		t.Fatalf("EnsurePaths changed a deliberate admin-only config: %q / %q", adminOnly.Panel.URLPath, adminOnly.Panel.AdminPath)
	}
}

func TestPanelPort(t *testing.T) {
	c := Default()
	c.Panel.Listen = ":12345"
	if got := c.PanelPort(); got != 12345 {
		t.Errorf("PanelPort(:12345) = %d, want 12345", got)
	}
	c.Panel.Listen = "127.0.0.1:8443"
	if got := c.PanelPort(); got != 8443 {
		t.Errorf("PanelPort(127.0.0.1:8443) = %d, want 8443", got)
	}
	c.Panel.Listen = ""
	if got := c.PanelPort(); got != DefaultPanelPort {
		t.Errorf("PanelPort(empty) = %d, want %d", got, DefaultPanelPort)
	}
}

func TestPanelURLUsesListenPort(t *testing.T) {
	c := Default()
	c.Panel.Listen = ":5231"
	c.Panel.PublicIP = "203.0.113.10"
	if got := c.PanelURL("/abc"); got != "https://203.0.113.10:5231/abc" {
		t.Errorf("PanelURL = %q, want port from listen", got)
	}
}

func TestGatewayFromSubnet(t *testing.T) {
	cases := []struct{ subnet, want string }{
		{"10.115.0.0/24", "10.115.0.1"},
		{"10.42.0.0/24", "10.42.0.1"},
		{"10.115.0.0/16", "10.115.0.1"}, // /16 still gets .1 (not used by scheme)
		{"garbage", ""},
		{"2001:db8::/64", ""},
	}
	for _, c := range cases {
		if got := GatewayFromSubnet(c.subnet); got != c.want {
			t.Errorf("GatewayFromSubnet(%q) = %q, want %q", c.subnet, got, c.want)
		}
	}
}

func TestRandomPanelPort(t *testing.T) {
	p, err := RandomPanelPort()
	if err != nil {
		t.Fatalf("RandomPanelPort: %v", err)
	}
	if p < PanelPortMin || p > PanelPortMax {
		t.Errorf("RandomPanelPort = %d, want in [%d, %d]", p, PanelPortMin, PanelPortMax)
	}
}

func TestFillAutoIPv4SubnetEnv(t *testing.T) {
	t.Setenv("VPSMGR_IPV4_SUBNET", "10.115.0.0/24")
	c := Default()
	// Pre-set the auto-detected fields so the test does no network detection.
	c.Net.ExtIF = "eth0"
	c.Panel.PublicIP = "203.0.113.10"
	if err := c.FillAuto(); err != nil {
		t.Fatalf("FillAuto: %v", err)
	}
	if c.Net.Subnet != "10.115.0.0/24" {
		t.Errorf("subnet = %q, want 10.115.0.0/24", c.Net.Subnet)
	}
	if c.Net.Gateway != "10.115.0.1" {
		t.Errorf("gateway = %q, want 10.115.0.1", c.Net.Gateway)
	}
}

func TestV4PolicyYAMLCompatibility(t *testing.T) {
	cases := []struct {
		raw  string
		want V4Policy
	}{
		{"v4_forward: true", V4Direct},
		{"v4_forward: false", V4Off},
		{`v4_forward: "false"`, V4Off},
		{"v4_forward: web-only", V4WebOnly},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			c := Default()
			if err := yaml.Unmarshal([]byte("net:\n  "+tc.raw+"\n"), c); err != nil {
				t.Fatal(err)
			}
			if c.Net.V4Forward != tc.want {
				t.Fatalf("policy = %q, want %q", c.Net.V4Forward, tc.want)
			}
		})
	}
	if err := yaml.Unmarshal([]byte("net:\n  v4_forward: invalid\n"), Default()); err == nil {
		t.Fatal("invalid v4_forward accepted")
	}
	out := MustYAML(&Config{Net: NetCfg{V4Forward: V4WebOnly}})
	if !strings.Contains(string(out), "v4_forward: web-only") {
		t.Fatalf("web-only did not round-trip: %s", out)
	}
}

func TestFillAutoV4ForwardEnv(t *testing.T) {
	c := Default()
	c.Net.ExtIF = "eth0"
	c.Panel.PublicIP = "203.0.113.10"
	t.Setenv("VPSMGR_V4_FORWARD", "0")
	if err := c.FillAuto(); err != nil {
		t.Fatalf("FillAuto: %v", err)
	}
	if c.Net.V4Forward != V4Off {
		t.Error("V4Forward should be false when VPSMGR_V4_FORWARD=0")
	}
}

func TestParseSlotRangeLegacy(t *testing.T) {
	// Legacy parser kept only for migrating pre-user_ports configs.
	cases := []struct {
		in     string
		lo, hi int
	}{
		{"2-201", 2, 201},
		{"6-201", 6, 201},
		{"2-100", 2, 100},
		{"20-100", 20, 100},
		{"201-201", 201, 201},
	}
	for _, c := range cases {
		lo, hi, err := ParseSlotRange(c.in)
		if err != nil {
			t.Errorf("ParseSlotRange(%q): unexpected error: %v", c.in, err)
			continue
		}
		if lo != c.lo || hi != c.hi {
			t.Errorf("ParseSlotRange(%q) = %d-%d, want %d-%d", c.in, lo, hi, c.lo, c.hi)
		}
	}
	bad := []string{
		"", " ", "1-201", "2-202", "2-300", "0-200", "3-2", "201-2",
		"x-y", "2--201", "2-201-", "-2-201", "2.5-201", "201-200", "2-201 extra",
		"2", "2,201", "0-0", "300-301", "-201", "2-", "2..201",
	}
	for _, s := range bad {
		if _, _, err := ParseSlotRange(s); err == nil {
			t.Errorf("ParseSlotRange(%q): expected error", s)
		}
	}
}

func TestLegacySlotRangeToUserPorts(t *testing.T) {
	cases := []struct {
		slot string
		want string
	}{
		{"2-201", "10000-29999"}, // full range
		{"2-100", "10000-19899"}, // 99 blocks
		{"102-201", "20000-29999"},
		{"201-201", "29900-29999"}, // single block
		{"6-201", "10400-29999"},
	}
	for _, c := range cases {
		got, err := LegacySlotRangeToUserPorts(c.slot)
		if err != nil {
			t.Errorf("LegacySlotRangeToUserPorts(%q): %v", c.slot, err)
			continue
		}
		if got != c.want {
			t.Errorf("LegacySlotRangeToUserPorts(%q) = %q, want %q", c.slot, got, c.want)
		}
	}
}

func TestParseUserPorts(t *testing.T) {
	cases := []struct {
		in    string
		count int
		canon string // canonical form; "" = must be rejected
	}{
		{"10000-29999", 200, "10000-29999"},                           // default full range
		{"10000-20000", 100, "10000-19999"},                           // hi rounds down to block end <= b (never exceeds b)
		{"10000-15000", 50, "10000-14999"},                            // boundary test: 15000 rounds down to 14999, not 15099
		{"10001-29998", 198, "10100-29899"},                           // inward-aligned both ends (29998 rounds down to 29899)
		{"10001-29999", 199, "10100-29999"},                           // inward-aligned low end only
		{"10001-10099", 0, ""},                                        // narrower than one block
		{"10000-10099", 1, "10000-10099"},                             // exactly one block
		{"10000-20000, 25000-30000", 150, "10000-19999, 25000-29999"}, // discontiguous
		{"10000-15000, 14000-20000", 100, "10000-19999"},              // overlapping → merged
		{"5000-15000", 50, "10000-14999"},                             // lo below domain clamps to 10000
		{"20000-40000", 100, "20000-29999"},                           // hi above domain clamps to 29999
		{"10000-10000", 0, ""},                                        // single port has no whole block
		{"5000-6000", 0, ""},                                          // fully outside domain
		{"10000-20000, 21000-21099", 101, "10000-19999, 21000-21099"}, // adjacent not merged (gap 100)
	}
	for _, c := range cases {
		rs, err := ParseUserPorts(c.in)
		if c.canon == "" {
			if err == nil {
				t.Errorf("ParseUserPorts(%q): expected error, got %v", c.in, rs)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseUserPorts(%q): unexpected error: %v", c.in, err)
			continue
		}
		n := 0
		for _, r := range rs {
			n += (r.Hi - r.Lo + 1) / 100
		}
		if n != c.count {
			t.Errorf("ParseUserPorts(%q) block count = %d, want %d", c.in, n, c.count)
		}
		if got := CanonicalUserPorts(c.in); got != c.canon {
			t.Errorf("CanonicalUserPorts(%q) = %q, want %q", c.in, got, c.canon)
		}
		// Every returned range must be whole-hundred aligned.
		for _, r := range rs {
			if r.Lo%100 != 0 || (r.Hi+1)%100 != 0 {
				t.Errorf("ParseUserPorts(%q) range %d-%d not whole-hundred aligned", c.in, r.Lo, r.Hi)
			}
			if r.Lo < UserPortBase || r.Hi > UserPortMax {
				t.Errorf("ParseUserPorts(%q) range %d-%d outside domain", c.in, r.Lo, r.Hi)
			}
		}
	}

	bad := []string{
		"", " ", "x-y", "10000-20000 extra", "10000", "10000-20000-30000",
		"20000-10000", // lo > hi
		"10000-x",
	}
	for _, s := range bad {
		if _, err := ParseUserPorts(s); err == nil {
			t.Errorf("ParseUserPorts(%q): expected error", s)
		}
	}
}

func TestUserPortCount(t *testing.T) {
	cfg := Default()
	if n := cfg.UserPortCount(); n != 200 {
		t.Errorf("default UserPortCount() = %d, want 200", n)
	}
	cfg.Net.UserPorts = "10000-20000, 25000-30000"
	if n := cfg.UserPortCount(); n != 150 {
		t.Errorf("UserPortCount() = %d, want 150", n)
	}
	cfg.Net.UserPorts = "garbage"
	if n := cfg.UserPortCount(); n != 200 {
		t.Errorf("garbage UserPortCount() = %d, want fallback 200", n)
	}
}

func TestFillAutoUserPortsEnv(t *testing.T) {
	c := Default()
	c.Net.ExtIF = "eth0"
	c.Panel.PublicIP = "203.0.113.10"
	t.Setenv("VPSMGR_USER_PORTS", "10000-20000, 25000-30000")
	if err := c.FillAuto(); err != nil {
		t.Fatalf("FillAuto: %v", err)
	}
	if c.Net.UserPorts != "10000-19999, 25000-29999" {
		t.Errorf("user_ports = %q, want canonical 10000-19999, 25000-29999", c.Net.UserPorts)
	}

	// A bad env value must fail loudly, not silently default.
	t.Setenv("VPSMGR_USER_PORTS", "5000-6000")
	if err := Default().FillAuto(); err == nil {
		t.Error("FillAuto with VPSMGR_USER_PORTS=5000-6000 should fail (no usable block)")
	}
}

func TestLoadLegacySlotRangeMigration(t *testing.T) {
	// A config written before net.user_ports carries only slot_range. Loading
	// it must convert it to user_ports once (the migration reads the raw file
	// because Default() pre-fills user_ports with the full range).
	write := func(body string) string {
		dir := t.TempDir()
		p := dir + "/config.yaml"
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("VPSMGR_CONFIG", p)
		return p
	}

	write(`panel:
  listen: ":8443"
net:
  subnet: "10.115.0.0/24"
  slot_range: "6-201"
  v4_forward: true
`)
	// ExtIF/public_ip detection must be overridable so FillAuto doesn't touch
	// the network. Set env for a stable public IP and eth0.
	t.Setenv("VPSMGR_V4_FORWARD", "1")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Net.UserPorts != "10400-29999" {
		t.Errorf("legacy slot_range migration = %q, want 10400-29999", c.Net.UserPorts)
	}

	// A modern config with user_ports must NOT be touched by a legacy key.
	write(`panel:
  listen: ":8443"
net:
  subnet: "10.115.0.0/24"
  user_ports: "10000-20000, 25000-30000"
  slot_range: "6-201"
  v4_forward: true
`)
	c2, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c2.Net.UserPorts != "10000-20000, 25000-30000" {
		t.Errorf("user_ports should win over slot_range; got %q", c2.Net.UserPorts)
	}
}

func TestFillAutoHaproxyEnv(t *testing.T) {
	c := Default()
	c.Net.ExtIF = "eth0"
	c.Panel.PublicIP = "203.0.113.10"
	// Install-time force-off (80/443 conflict) must write net.haproxy false.
	t.Setenv("VPSMGR_HAPROXY", "0")
	if err := c.FillAuto(); err != nil {
		t.Fatalf("FillAuto: %v", err)
	}
	if c.Net.Haproxy {
		t.Error("net.haproxy should be false when VPSMGR_HAPROXY=0")
	}
	// VPSMGR_HAPROXY=1 forces it on.
	c.Net.Haproxy = false
	t.Setenv("VPSMGR_HAPROXY", "true")
	if err := c.FillAuto(); err != nil {
		t.Fatalf("FillAuto: %v", err)
	}
	if !c.Net.Haproxy {
		t.Error("net.haproxy should be true when VPSMGR_HAPROXY=true")
	}
}

// TestFillAutoLegacyTraefikEnv covers the upgrade path from an old installer:
// a pre-rename checkout exported VPSMGR_TRAEFIK for the very same knob. It is
// honored only as a fallback, so an operator running an old script against the
// new binary still gets the state they asked for.
func TestFillAutoLegacyTraefikEnv(t *testing.T) {
	c := Default()
	c.Net.ExtIF = "eth0"
	c.Panel.PublicIP = "203.0.113.10"
	t.Setenv("VPSMGR_TRAEFIK", "0")
	if err := c.FillAuto(); err != nil {
		t.Fatalf("FillAuto: %v", err)
	}
	if c.Net.Haproxy {
		t.Error("legacy VPSMGR_TRAEFIK=0 should still disable the domain proxy")
	}

	// The new spelling wins when both are present.
	c.Net.Haproxy = false
	t.Setenv("VPSMGR_HAPROXY", "1")
	if err := c.FillAuto(); err != nil {
		t.Fatalf("FillAuto: %v", err)
	}
	if !c.Net.Haproxy {
		t.Error("VPSMGR_HAPROXY must take precedence over the legacy VPSMGR_TRAEFIK")
	}
}

// TestLoadLegacyTraefikKey covers a config file written by a Traefik-era
// install: it carries `traefik:` where the new tree writes `haproxy:`. The
// value semantics are identical, so the old value must be honored verbatim and
// then dropped from the file on the next save.
func TestLoadLegacyTraefikKey(t *testing.T) {
	write := func(t *testing.T, body string) string {
		t.Helper()
		p := t.TempDir() + "/config.yaml"
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("VPSMGR_CONFIG", p)
		t.Setenv("VPSMGR_V4_FORWARD", "1")
		return p
	}

	// 1. Only the legacy key: its value is used, and a save rewrites the file
	//    with the new key only.
	p := write(t, `panel:
  listen: ":8443"
net:
  subnet: "10.115.0.0/24"
  v4_forward: true
  traefik: false
`)
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Net.Haproxy {
		t.Error("legacy traefik: false must be honored as net.haproxy=false")
	}
	if c.Net.LegacyTraefik != nil {
		t.Error("legacy key must be cleared after Load so it cannot be written back")
	}
	if err := Save(c); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "traefik:") {
		t.Errorf("saved config still carries the legacy key:\n%s", b)
	}
	if !strings.Contains(string(b), "haproxy: false") {
		t.Errorf("saved config missing the new key:\n%s", b)
	}

	// 2. Only the new key: honored as-is.
	write(t, `panel:
  listen: ":8443"
net:
  subnet: "10.115.0.0/24"
  v4_forward: true
  haproxy: false
`)
	c, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Net.Haproxy {
		t.Error("haproxy: false must be honored")
	}

	// 3. Both keys (a half-migrated file, or a hand edit): the NEW key wins.
	write(t, `panel:
  listen: ":8443"
net:
  subnet: "10.115.0.0/24"
  v4_forward: true
  haproxy: true
  traefik: false
`)
	c, err = Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.Net.Haproxy {
		t.Error("haproxy: true must win over a stale traefik: false")
	}
}
