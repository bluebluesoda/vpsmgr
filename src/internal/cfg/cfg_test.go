package cfg

import (
	"strings"
	"testing"
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

func TestSlotRange(t *testing.T) {
	// Valid: any sub-range of [2,201] with lo <= hi, including the default.
	cases := []struct {
		in       string
		lo, hi   int
		slots, min, max int // min/max = idx bounds (octet-1)
	}{
		{"2-201", 2, 201, 200, 1, 200},
		{"2-201", 2, 201, 200, 1, 200},
		{"6-201", 6, 201, 196, 5, 200},
		{"2-100", 2, 100, 99, 1, 99},
		{"20-100", 20, 100, 81, 19, 99},
		{"201-201", 201, 201, 1, 200, 200},
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
		cfg := Default()
		cfg.Net.SlotRange = c.in
		if n := cfg.SlotCount(); n != c.slots {
			t.Errorf("SlotCount(%q) = %d, want %d", c.in, n, c.slots)
		}
		iMin, iMax := cfg.SlotIdxBounds()
		if iMin != c.min || iMax != c.max {
			t.Errorf("SlotIdxBounds(%q) = %d..%d, want %d..%d", c.in, iMin, iMax, c.min, c.max)
		}
	}

	// Invalid: expanding beyond the default edges, lo > hi, malformed, non-int.
	// (2-200 and 200-201 ARE valid sub-ranges; expanding past the edges is not.)
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

func TestFillAutoSlotRangeEnv(t *testing.T) {
	c := Default()
	c.Net.ExtIF = "eth0"
	c.Panel.PublicIP = "203.0.113.10"
	t.Setenv("VPSMGR_SLOT_RANGE", "6-201")
	if err := c.FillAuto(); err != nil {
		t.Fatalf("FillAuto: %v", err)
	}
	if c.Net.SlotRange != "6-201" {
		t.Errorf("slot_range = %q, want 6-201", c.Net.SlotRange)
	}

	// A bad env value must fail loudly, not silently default.
	t.Setenv("VPSMGR_SLOT_RANGE", "2-300")
	if err := Default().FillAuto(); err == nil {
		t.Error("FillAuto with VPSMGR_SLOT_RANGE=2-300 should fail")
	}
}

func TestFillAutoTraefikEnv(t *testing.T) {
	c := Default()
	c.Net.ExtIF = "eth0"
	c.Panel.PublicIP = "203.0.113.10"
	// Install-time force-off (80/443 conflict) must write net.traefik false.
	t.Setenv("VPSMGR_TRAEFIK", "0")
	if err := c.FillAuto(); err != nil {
		t.Fatalf("FillAuto: %v", err)
	}
	if c.Net.Traefik {
		t.Error("net.traefik should be false when VPSMGR_TRAEFIK=0")
	}
	// VPSMGR_TRAEFIK=1 forces it on.
	c.Net.Traefik = false
	t.Setenv("VPSMGR_TRAEFIK", "true")
	if err := c.FillAuto(); err != nil {
		t.Fatalf("FillAuto: %v", err)
	}
	if !c.Net.Traefik {
		t.Error("net.traefik should be true when VPSMGR_TRAEFIK=true")
	}
}
