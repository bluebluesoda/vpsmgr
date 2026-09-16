package mgr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
	"vpsmgr/internal/hpx"
)

// setupDomainTest builds a Manager whose proxy publisher writes into a temp
// directory and whose configuration check is stubbed to succeed (the real one
// runs /usr/sbin/haproxy -c, which a test box does not have).
//
// The returned dir is the publisher root; call currentDomains to read back what
// the running proxy would load.
func setupDomainTest(t *testing.T, dir string) (*Manager, *db.DB, string) {
	t.Helper()
	c := cfg.Default()
	c.Net.V4Forward = true
	c.Net.Haproxy = true
	if dir == "" {
		dir = t.TempDir()
	}
	t.Setenv("VPSMGR_HAPROXY_DIR", dir)
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	m := New(c, d)
	p := hpx.New(c)
	p.SetCheck(func(string) error { return nil })
	m.SetProxy(p)
	return m, d, "alice"
}

// setupFailingPublish builds a manager whose publisher FAILS validation, to
// exercise the DB rollback paths.
func setupFailingPublish(t *testing.T) (*Manager, *db.DB, string) {
	t.Helper()
	c := cfg.Default()
	c.Net.V4Forward = true
	c.Net.Haproxy = true
	t.Setenv("VPSMGR_HAPROXY_DIR", t.TempDir())
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if _, err := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	m := New(c, d)
	p := hpx.New(c)
	p.SetCheck(func(string) error { return os.ErrInvalid })
	m.SetProxy(p)
	return m, d, "alice"
}

// currentDomains returns the text of the domain fragment in the generation
// "current" points at — exactly what a reload would load.
func currentDomains(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "current", "domains.cfg"))
	if err != nil {
		t.Fatalf("read published domains: %v", err)
	}
	return string(b)
}

// currentMap returns one of the generation's routing maps. Routing lives there
// (host -> backend, SNI -> backend), not in the domain fragment.
func currentMap(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "current", "maps", name))
	if err != nil {
		t.Fatalf("read published %s: %v", name, err)
	}
	return string(b)
}

func TestAddDomainPublishesRoute(t *testing.T) {
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
	got := currentDomains(t, dir)
	// 443 backend carries PROXY protocol v2; 80 backend never does.
	if !strings.Contains(got, "backend be_t_api_example_com") {
		t.Errorf("missing TCP backend:\n%s", got)
	}
	if !strings.Contains(got, "server s  10.115.0.2:443 check send-proxy-v2") {
		t.Errorf("missing send-proxy-v2 on the 443 backend:\n%s", got)
	}
	if strings.Contains(got, "10.115.0.2:80 check send-proxy-v2") {
		t.Errorf("PROXY protocol must never be sent on :80:\n%s", got)
	}
	if !strings.Contains(currentMap(t, dir, "http.map"), "api.example.com be_h_api_example_com") {
		t.Errorf("missing HTTP routing entry:\n%s", currentMap(t, dir, "http.map"))
	}
}

func TestAddDomainWithoutProxyProtocol(t *testing.T) {
	dir := t.TempDir()
	m, _, name := setupDomainTest(t, dir)
	if err := m.AddDomain(name, "example.com", false); err != nil {
		t.Fatal(err)
	}
	got := currentDomains(t, dir)
	if strings.Contains(got, "send-proxy-v2") {
		t.Errorf("send-proxy-v2 present although the domain has PROXY off:\n%s", got)
	}
	if !strings.Contains(currentMap(t, dir, "sni.map"), "example.com be_t_example_com") {
		t.Errorf("missing SNI routing entry:\n%s", currentMap(t, dir, "sni.map"))
	}
}

// A crash between the DB write and the publish must not leave a domain that the
// proxy serves but the panel does not know about.
func TestAddDomainRollsBackDBOnPublishFailure(t *testing.T) {
	m, d, name := setupFailingPublish(t)
	if err := m.AddDomain(name, "example.com", false); err == nil {
		t.Fatal("AddDomain should fail when the publish fails")
	}
	if _, err := d.GetDomainByDomain("example.com"); err == nil {
		t.Fatal("DB row not rolled back after a publish failure")
	}
}

func TestSetDomainProtocolRollsBackOnPublishFailure(t *testing.T) {
	dir := t.TempDir()
	m, d, name := setupDomainTest(t, dir)
	if err := m.AddDomain(name, "example.com", false); err != nil {
		t.Fatal(err)
	}
	// Swap in a failing publisher AFTER the domain exists, so only the toggle
	// is exercised.
	bad := hpx.New(cfg.Default())
	bad.SetCheck(func(string) error { return os.ErrInvalid })
	m.SetProxy(bad)
	if err := m.SetDomainProtocol(name, "example.com", true); err == nil {
		t.Fatal("SetDomainProtocol should fail when the publish fails")
	}
	dmn, err := d.GetDomainByDomain("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if dmn.ProxyProtocol {
		t.Fatal("proxy_protocol not rolled back after a publish failure")
	}
}

func TestDelDomainRemovesRouteAndDB(t *testing.T) {
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
	// The republished generation must no longer route the domain.
	got := currentDomains(t, dir)
	if strings.Contains(got, "be_h_example_com") || strings.Contains(got, "be_t_example_com") {
		t.Fatalf("domain still routed after delete:\n%s", got)
	}
}

// SyncAllDomains must be able to reconstruct everything from the DB alone: a
// wiped proxy directory is repaired on the next sync.
func TestSyncAllDomainsRebuildsFromDB(t *testing.T) {
	dir := t.TempDir()
	m, d, name := setupDomainTest(t, dir)
	if err := m.AddDomain(name, "example.com", false); err != nil {
		t.Fatal(err)
	}
	// Simulate total loss of the published layout.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	p := hpx.New(cfg.Default())
	p.SetCheck(func(string) error { return nil })
	m.SetProxy(p)
	if err := m.SyncAllDomains(); err != nil {
		t.Fatalf("sync after wipe: %v", err)
	}
	got := currentDomains(t, dir)
	if !strings.Contains(got, "backend be_h_example_com") {
		t.Fatalf("domain backend not rebuilt from the DB:\n%s", got)
	}
	if !strings.Contains(currentMap(t, dir, "http.map"), "example.com be_h_example_com") {
		t.Fatalf("domain route not rebuilt from the DB:\n%s", currentMap(t, dir, "http.map"))
	}
	// And the DB is still the only source of truth for it.
	if _, err := d.GetDomainByDomain("example.com"); err != nil {
		t.Fatal(err)
	}
}

// A domain deleted directly from the DB (an out-of-band edit) must disappear
// from the next published generation: generations are rebuilt, never patched.
func TestSyncAllDomainsDropsUnknownDomains(t *testing.T) {
	dir := t.TempDir()
	m, d, name := setupDomainTest(t, dir)
	if err := m.AddDomain(name, "example.com", false); err != nil {
		t.Fatal(err)
	}
	u, err := d.GetUserByName(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteDomain(u.ID, "example.com"); err != nil {
		t.Fatal(err)
	}
	if err := m.SyncAllDomains(); err != nil {
		t.Fatal(err)
	}
	if got := currentDomains(t, dir); strings.Contains(got, "be_h_example_com") {
		t.Fatalf("a domain no longer in the DB is still routed:\n%s", got)
	}
}
