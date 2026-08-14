package mgr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
)

func setupDomainTest(t *testing.T, traefikDir string) (*Manager, *db.DB, string) {
	t.Helper()
	c := cfg.Default()
	c.Net.V4Forward = true
	if traefikDir != "" {
		os.Setenv("VPSMGR_TRAEFIK_DIR", traefikDir)
		t.Cleanup(func() { os.Unsetenv("VPSMGR_TRAEFIK_DIR") })
	}
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	return New(c, d), d, "alice"
}

func TestAddDomainWritesDBAndYAML(t *testing.T) {
	dir := t.TempDir()
	m, d, name := setupDomainTest(t, dir)
	if err := m.AddDomain(name, "api.example.com", true); err != nil {
		t.Fatal(err)
	}
	// DB row present with the flag.
	dmn, err := d.GetDomainByDomain("api.example.com")
	if err != nil {
		t.Fatalf("db row missing: %v", err)
	}
	if !dmn.ProxyProtocol {
		t.Error("proxy_protocol not persisted")
	}
	// YAML file written with the proxyProtocol block.
	b, err := os.ReadFile(filepath.Join(dir, "api.example.com.yaml"))
	if err != nil {
		t.Fatalf("yaml file missing: %v", err)
	}
	got := string(b)
	if !strings.Contains(got, "proxyProtocol:\n          version: 2") || !strings.Contains(got, "u-api_example_com") {
		t.Errorf("unexpected yaml:\n%s", got)
	}
}

func TestAddDomainRollsBackDBOnTraefikFailure(t *testing.T) {
	// The traefik dir points at a nonexistent path, so WriteDomain fails.
	m, d, name := setupDomainTest(t, filepath.Join(t.TempDir(), "no", "such", "dir"))
	if err := m.AddDomain(name, "example.com", false); err == nil {
		t.Fatal("AddDomain should fail when the traefik write fails")
	}
	if _, err := d.GetDomainByDomain("example.com"); err == nil {
		t.Fatal("DB row not rolled back after a traefik write failure")
	}
}

func TestSetDomainProtocolRollsBackOnTraefikFailure(t *testing.T) {
	m, d, name := setupDomainTest(t, t.TempDir())
	if err := m.AddDomain(name, "example.com", false); err != nil {
		t.Fatal(err)
	}
	// A manager whose traefik dir fails on write (captured at New).
	os.Setenv("VPSMGR_TRAEFIK_DIR", filepath.Join(t.TempDir(), "no", "such", "dir"))
	t.Cleanup(func() { os.Unsetenv("VPSMGR_TRAEFIK_DIR") })
	m2 := New(cfg.Default(), d)
	if err := m2.SetDomainProtocol(name, "example.com", true); err == nil {
		t.Fatal("SetDomainProtocol should fail when the traefik write fails")
	}
	dmn, err := d.GetDomainByDomain("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if dmn.ProxyProtocol {
		t.Fatal("proxy_protocol not rolled back after a traefik write failure")
	}
}

func TestDelDomainRemovesYAMLAndDB(t *testing.T) {
	dir := t.TempDir()
	m, d, name := setupDomainTest(t, dir)
	if err := m.AddDomain(name, "example.com", false); err != nil {
		t.Fatal(err)
	}
	if err := m.DelDomain(name, "example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetDomainByDomain("example.com"); err == nil {
		t.Fatal("db row still present after delete")
	}
	if _, err := os.Stat(filepath.Join(dir, "example.com.yaml")); !os.IsNotExist(err) {
		t.Fatalf("yaml file still present: %v", err)
	}
}
