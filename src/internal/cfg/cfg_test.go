package cfg

import (
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

func TestVersionFieldsRoundTrip(t *testing.T) {
	c := Default()
	c.InstalledVersion = "0.2.1"
	c.UninstalledVersion = "0.2.0"
	b, err := yaml.Marshal(c)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got := Default()
	if err := yaml.Unmarshal(b, got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.InstalledVersion != "0.2.1" {
		t.Errorf("installed_version = %q, want 0.2.1", got.InstalledVersion)
	}
	if got.UninstalledVersion != "0.2.0" {
		t.Errorf("uninstalled_version = %q, want 0.2.0", got.UninstalledVersion)
	}
	if !strings.Contains(string(b), "installed_version: 0.2.1") {
		t.Errorf("marshaled yaml missing installed_version:\n%s", b)
	}
	// Empty versions must be omitted (omitempty) so existing configs stay clean.
	empty := Default()
	if b, _ := yaml.Marshal(empty); strings.Contains(string(b), "installed_version") || strings.Contains(string(b), "uninstalled_version") {
		t.Errorf("empty version fields should be omitted:\n%s", b)
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

func TestFillAutoV4ForwardEnv(t *testing.T) {
	c := Default()
	c.Net.ExtIF = "eth0"
	c.Panel.PublicIP = "203.0.113.10"
	t.Setenv("VPSMGR_V4_FORWARD", "0")
	if err := c.FillAuto(); err != nil {
		t.Fatalf("FillAuto: %v", err)
	}
	if c.Net.V4Forward {
		t.Error("V4Forward should be false when VPSMGR_V4_FORWARD=0")
	}
}
