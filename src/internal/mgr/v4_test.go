package mgr

import (
	"path/filepath"
	"testing"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
)

// TestAddDomainRejectedWhenV4Off: with v4_forward=false the domain proxy is not
// offered, so adding a domain must be rejected before anything is published.
func TestAddDomainRejectedWhenV4Off(t *testing.T) {
	c := cfg.Default()
	c.Net.V4Forward = cfg.V4Off
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.CreateUser("alice", "x", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	m := New(c, d)
	if err := m.AddDomain("alice", "example.com", false); err == nil {
		t.Fatal("AddDomain should be rejected when v4_forward is false")
	}
}

func TestAddDomainRejectedWhenProxyOff(t *testing.T) {
	c := cfg.Default()
	c.Net.Haproxy = false
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.CreateUser("alice", "x", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	m := New(c, d)
	if err := m.AddDomain("alice", "example.com", false); err == nil {
		t.Fatal("AddDomain should be rejected when the domain proxy is disabled")
	}
}

func TestHaproxyLive(t *testing.T) {
	c := cfg.Default()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	m := New(c, d)
	if !m.HaproxyLive() {
		t.Fatal("HaproxyLive should fall back to the config default")
	}
	if err := d.SetSetting(db.SettingHaproxy, "false"); err != nil {
		t.Fatal(err)
	}
	if m.HaproxyLive() {
		t.Fatal("HaproxyLive should read false from the DB setting")
	}
}

// TestV4ForwardLive: the panel's long-running process must reflect a policy
// change made through `vps config set` even though its config still says otherwise.
func TestV4ForwardLive(t *testing.T) {
	c := cfg.Default()
	c.Net.V4Forward = cfg.V4Direct
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	m := New(c, d)
	if m.LiveV4Policy() != cfg.V4Direct || !m.V4ForwardLive() {
		t.Fatal("live policy should fall back to the config default")
	}

	for raw, want := range map[string]cfg.V4Policy{
		"1": cfg.V4Direct, "0": cfg.V4Off, "false": cfg.V4Off, "web-only": cfg.V4WebOnly,
	} {
		if err := d.SetSetting(db.SettingV4Forward, raw); err != nil {
			t.Fatal(err)
		}
		if got := m.LiveV4Policy(); got != want {
			t.Errorf("DB %q policy = %q, want %q", raw, got, want)
		}
	}
	if err := d.SetSetting(db.SettingV4Forward, "web-only"); err != nil {
		t.Fatal(err)
	}
	if !m.LiveV4Capabilities().ShowPublicIPv4 || m.LiveV4Capabilities().DirectForwarding {
		t.Fatal("web-only should show public IPv4 without direct forwarding")
	}
	if !m.LiveDomainProxyEnabled() {
		t.Fatal("web-only with HAProxy enabled should allow domains")
	}

	if err := d.SetSetting(db.SettingV4Forward, "garbage"); err != nil {
		t.Fatal(err)
	}
	if m.LiveV4Policy() != cfg.V4Off || m.LiveDomainProxyEnabled() {
		t.Fatal("malformed DB policy should fail closed")
	}
}
