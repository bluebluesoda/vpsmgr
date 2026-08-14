package cfg

import (
	"strings"
	"testing"
)

func TestFieldForUnknown(t *testing.T) {
	if FieldFor("nope") != nil {
		t.Fatal("unknown key should not resolve")
	}
	if FieldFor("net.subnet") == nil {
		t.Fatal("net.subnet should resolve")
	}
}

func TestFieldKindsCovered(t *testing.T) {
	// The registry must classify every key it knows; nothing may fall through.
	for _, f := range Fields {
		if f.Key == "" || f.Get == nil || f.Assign == nil {
			t.Fatalf("field %q incomplete (key/get/assign)", f.Key)
		}
		if f.Kind.String() == "?" || f.Apply.String() == "?" {
			t.Fatalf("field %q has unknown kind/apply", f.Key)
		}
	}
}

func TestFieldValueReadsConfig(t *testing.T) {
	c := Default()
	c.Net.Subnet = "10.42.0.0/24"
	c.Panel.SessionDays = 7
	if v := FieldValue(c, "net.subnet"); v != "10.42.0.0/24" {
		t.Errorf("net.subnet = %q", v)
	}
	if v := FieldValue(c, "panel.session_days"); v != "7" {
		t.Errorf("panel.session_days = %q", v)
	}
	if v := FieldValue(c, "net.v4_forward"); v != "true" {
		t.Errorf("net.v4_forward default = %q", v)
	}
}

func TestAssignValidators(t *testing.T) {
	c := Default()

	if err := FieldFor("panel.session_days").Assign(c, "5"); err != nil {
		t.Fatalf("session_days=5: %v", err)
	}
	if c.Panel.SessionDays != 5 {
		t.Errorf("session_days = %d", c.Panel.SessionDays)
	}
	if err := FieldFor("panel.session_days").Assign(c, "0"); err == nil {
		t.Error("session_days=0 accepted")
	}
	if err := FieldFor("panel.session_days").Assign(c, "x"); err == nil {
		t.Error("session_days=x accepted")
	}

	for _, v := range []string{"true", "1", "on", "false", "0", "off"} {
		if err := FieldFor("net.v4_forward").Assign(c, v); err != nil {
			t.Errorf("v4_forward=%q: %v", v, err)
		}
	}
	if err := FieldFor("net.v4_forward").Assign(c, "maybe"); err == nil {
		t.Error("v4_forward=maybe accepted")
	}

	if err := FieldFor("net.ipv6_subnet").Assign(c, "2001:db8::/64"); err != nil {
		t.Fatalf("ipv6_subnet valid: %v", err)
	}
	if err := FieldFor("net.ipv6_subnet").Assign(c, "2001:db8::"); err == nil {
		t.Error("bare ipv6 address without prefix accepted")
	}
	if err := FieldFor("net.ipv6_subnet").Assign(c, ""); err != nil {
		t.Fatalf("ipv6_subnet empty (disable): %v", err)
	}
	if c.Net.IPv6Subnet != "" {
		t.Errorf("ipv6_subnet not cleared: %q", c.Net.IPv6Subnet)
	}

	if err := FieldFor("panel.listen").Assign(c, ":8443"); err != nil {
		t.Fatalf("listen valid: %v", err)
	}
	if err := FieldFor("panel.listen").Assign(c, "8443"); err == nil {
		t.Error("listen without port separator accepted")
	}
}

func TestImmutableAssignsRefuse(t *testing.T) {
	c := Default()
	for _, key := range []string{"net.subnet", "net.gateway", "incus.pool", "incus.bridge"} {
		if err := FieldFor(key).Assign(c, "whatever"); err == nil {
			t.Errorf("%s: immutable assign accepted", key)
		} else if !strings.Contains(err.Error(), "fixed at install") {
			t.Errorf("%s: unexpected error: %v", key, err)
		}
	}
}

func TestAdminPassHashManagedElsewhere(t *testing.T) {
	c := Default()
	if err := FieldFor("panel.admin_pass_hash").Assign(c, "x"); err == nil {
		t.Error("admin_pass_hash set via config accepted")
	}
	if v := FieldValue(c, "panel.admin_pass_hash"); !strings.Contains(v, "vps admin-passwd") {
		t.Errorf("admin_pass_hash list value = %q", v)
	}
}

func TestEditableClassification(t *testing.T) {
	cases := map[string]string{
		"panel.listen":          "yes",
		"panel.session_days":    "yes",
		"net.v4_forward":        "yes",
		"net.ipv6_subnet":       "yes",
		"panel.url_path":        "only-when-empty",
		"panel.admin_url_path":  "yes",
		"net.subnet":            "no",
		"net.gateway":           "no",
		"incus.pool":            "no",
		"incus.bridge":          "no",
		"panel.admin_pass_hash": "no",
	}
	for key, want := range cases {
		if got := FieldFor(key).Editable(); got != want {
			t.Errorf("Editable(%s) = %q, want %q", key, got, want)
		}
	}
}

func TestSecretPathValidators(t *testing.T) {
	c := Default()
	// too short
	if err := FieldFor("panel.url_path").Assign(c, "Short1"); err == nil {
		t.Error("short url_path accepted")
	}
	// bad charset
	if err := FieldFor("panel.url_path").Assign(c, "bad path with spaces"); err == nil {
		t.Error("url_path with spaces accepted")
	}
	// valid
	if err := FieldFor("panel.url_path").Assign(c, "Ab1_cdE-9x"); err != nil {
		t.Fatalf("valid url_path rejected: %v", err)
	}
	// collision with the other panel path
	c.Panel.AdminPath = "Xy-9ab_cdE"
	if err := FieldFor("panel.url_path").Assign(c, "Xy-9ab_cdE"); err == nil {
		t.Error("url_path equal to admin_url_path accepted")
	}
}

func TestAdminPathEditable(t *testing.T) {
	c := Default()
	// valid path set
	if err := FieldFor("panel.admin_url_path").Assign(c, "Xy-9ab_cdE"); err != nil {
		t.Fatalf("valid admin_url_path rejected: %v", err)
	}
	if c.Panel.AdminPath != "Xy-9ab_cdE" {
		t.Errorf("admin_url_path = %q", c.Panel.AdminPath)
	}
	// list shows the path when set, "disabled" when empty
	if v := FieldValue(c, "panel.admin_url_path"); v != "Xy-9ab_cdE" {
		t.Errorf("admin_url_path value = %q", v)
	}
	// empty value = disable the admin panel (allowed, not refused)
	if err := FieldFor("panel.admin_url_path").Assign(c, ""); err != nil {
		t.Fatalf("empty admin_url_path (disable) rejected: %v", err)
	}
	if c.Panel.AdminPath != "" {
		t.Errorf("admin_url_path not cleared: %q", c.Panel.AdminPath)
	}
	if v := FieldValue(c, "panel.admin_url_path"); v != "disabled" {
		t.Errorf("admin_url_path value when disabled = %q", v)
	}
	// still validated: too short / bad charset / collision with user path
	if err := FieldFor("panel.admin_url_path").Assign(c, "Short1"); err == nil {
		t.Error("short admin_url_path accepted")
	}
	if err := FieldFor("panel.admin_url_path").Assign(c, "bad path with spaces"); err == nil {
		t.Error("admin_url_path with spaces accepted")
	}
	c.Panel.URLPath = "Ab1_cdE-9x"
	if err := FieldFor("panel.admin_url_path").Assign(c, "Ab1_cdE-9x"); err == nil {
		t.Error("admin_url_path equal to url_path accepted")
	}
}

func TestIPValidators(t *testing.T) {
	c := Default()
	if err := FieldFor("panel.public_ip").Assign(c, "1.2.3.4"); err != nil {
		t.Fatalf("valid public_ip rejected: %v", err)
	}
	if c.Panel.PublicIP != "1.2.3.4" {
		t.Errorf("public_ip = %q", c.Panel.PublicIP)
	}
	if err := FieldFor("panel.public_ip").Assign(c, "not-an-ip"); err == nil {
		t.Error("invalid public_ip accepted")
	}
	// AUTO clears it so FillAuto re-detects on the next load.
	if err := FieldFor("panel.public_ip").Assign(c, "AUTO"); err != nil {
		t.Fatalf("AUTO public_ip rejected: %v", err)
	}
	if c.Panel.PublicIP != "" {
		t.Errorf("AUTO public_ip not cleared: %q", c.Panel.PublicIP)
	}
	// display_ip may be empty (fall back to public_ip).
	if err := FieldFor("panel.display_ip").Assign(c, ""); err != nil {
		t.Fatalf("empty display_ip rejected: %v", err)
	}
	if err := FieldFor("panel.display_ip").Assign(c, "2001:db8::1"); err != nil {
		t.Fatalf("valid IPv6 display_ip rejected: %v", err)
	}
	if err := FieldFor("panel.display_ip").Assign(c, "junk"); err == nil {
		t.Error("invalid display_ip accepted")
	}
}

func TestPanelDBApplyIsInstall(t *testing.T) {
	if got := FieldFor("panel.db").Apply; got != ApplyInstall {
		t.Errorf("panel.db apply = %v, want re-run vps install", got)
	}
	if err := FieldFor("panel.db").Assign(Default(), ""); err == nil {
		t.Error("empty db path accepted")
	}
}

func TestNonEmptyOperators(t *testing.T) {
	c := Default()
	for _, key := range []string{"panel.cert", "panel.key", "net.ext_if", "incus.image", "incus.socket"} {
		if err := FieldFor(key).Assign(c, ""); err == nil {
			t.Errorf("%s: empty accepted", key)
		}
	}
}
