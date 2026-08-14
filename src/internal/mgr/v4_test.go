package mgr

import (
	"path/filepath"
	"testing"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
)

// TestAddDomainRejectedWhenV4Off: with v4_forward=false the domain proxy is not
// offered, so adding a domain must be rejected before any traefik write.
func TestAddDomainRejectedWhenV4Off(t *testing.T) {
	c := cfg.Default()
	c.Net.V4Forward = false
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

// TestV4ForwardLive: the panel's long-running process must reflect a toggle
// made through `vps config set` (which writes the DB setting via
// ApplyV4State) even though its in-memory config still says otherwise.
func TestV4ForwardLive(t *testing.T) {
	c := cfg.Default()
	c.Net.V4Forward = true
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	m := New(c, d)
	if !m.V4ForwardLive() {
		t.Fatal("V4ForwardLive should fall back to the config default")
	}

	// A CLI toggle writes the DB setting; the in-memory config is untouched.
	if err := d.SetSetting(db.SettingV4Forward, "false"); err != nil {
		t.Fatal(err)
	}
	if m.V4ForwardLive() {
		t.Fatal("V4ForwardLive should read false from the DB setting")
	}

	// A malformed setting is treated as off (only explicit "true"/"1" enables),
	// so a bad DB value can never accidentally re-open domain-add.
	if err := d.SetSetting(db.SettingV4Forward, "garbage"); err != nil {
		t.Fatal(err)
	}
	if m.V4ForwardLive() {
		t.Fatal("V4ForwardLive should treat a malformed DB setting as off")
	}
}
