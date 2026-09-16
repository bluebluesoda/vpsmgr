package hpx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vpsmgr/internal/cfg"
)

// newTestProxy builds a publisher over a temp layout with a stubbed
// configuration check (the real one shells out to /usr/sbin/haproxy).
func newTestProxy(t *testing.T) *Proxy {
	t.Helper()
	t.Setenv("VPSMGR_HAPROXY_DIR", t.TempDir())
	c := cfg.Default()
	p := New(c)
	p.SetCheck(func(string) error { return nil })
	return p
}

// newRealProxy is like newTestProxy but with the real configuration check, so
// Validate() shells out to the binary. Skips when none is available.
func newRealProxy(t *testing.T) *Proxy {
	t.Helper()
	bin := os.Getenv("VPSMGR_TEST_HAPROXY_BIN")
	if bin == "" {
		t.Skip("VPSMGR_TEST_HAPROXY_BIN not set")
	}
	t.Setenv("VPSMGR_HAPROXY_DIR", t.TempDir())
	t.Setenv("VPSMGR_HAPROXY_BIN", bin)
	p := New(cfg.Default())
	// The entry config only exists on an installed host; render it for the
	// temp layout so haproxy resolves every include.
	if err := p.WriteEntryConfig(); err != nil {
		t.Fatal(err)
	}
	return p
}

func readCurrent(t *testing.T, p *Proxy, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(p.Dir(), "current", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

func TestSanitizeDomainCollisionFree(t *testing.T) {
	// Two domains that differ only by separator must not collide.
	if sanitizeDomain("example.com") == sanitizeDomain("example-com") {
		t.Fatal("example.com and example-com collide")
	}
	if got := sanitizeDomain("api.example.com"); got != "api_example_com" {
		t.Fatalf("sanitizeDomain = %q", got)
	}
}

func TestPublishRendersHTTPAndTLSBackends(t *testing.T) {
	p := newTestProxy(t)
	err := p.PublishAll([]Domain{
		{Domain: "api.example.com", IP: "10.115.0.2", ProxyProtocol: true},
		{Domain: "example.com", IP: "10.115.0.3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := readCurrent(t, p, "domains.cfg")

	// HTTP backend: plain reverse proxy to :80, no PROXY protocol.
	if !strings.Contains(got, "backend be_h_api_example_com\n    mode http\n    option forwardfor\n    server s  10.115.0.2:80 check") {
		t.Errorf("missing HTTP backend:\n%s", got)
	}
	// TLS backend with PROXY protocol v2 enabled.
	if !strings.Contains(got, "server s  10.115.0.2:443 check send-proxy-v2") {
		t.Errorf("missing send-proxy-v2 on the 443 backend:\n%s", got)
	}
	// TLS backend without it.
	if !strings.Contains(got, "server s  10.115.0.3:443 check\n") {
		t.Errorf("missing plain 443 backend:\n%s", got)
	}
	// Routing lives in the generation's maps: host -> backend, SNI -> backend.
	httpMap := readCurrent(t, p, "maps/http.map")
	if !strings.Contains(httpMap, "api.example.com be_h_api_example_com") ||
		!strings.Contains(httpMap, "example.com be_h_example_com") {
		t.Errorf("unexpected http map:\n%s", httpMap)
	}
	sniMap := readCurrent(t, p, "maps/sni.map")
	if !strings.Contains(sniMap, "api.example.com be_t_api_example_com") {
		t.Errorf("unexpected sni map:\n%s", sniMap)
	}
}

// A generation must describe the DOMAIN SET, not a delta: publishing a subset
// drops everything else, which is what makes "rebuild from the DB" safe.
func TestPublishReplacesWholeGeneration(t *testing.T) {
	p := newTestProxy(t)
	if err := p.PublishAll([]Domain{{Domain: "a.example.com", IP: "10.115.0.2"}}); err != nil {
		t.Fatal(err)
	}
	if err := p.PublishAll([]Domain{{Domain: "b.example.com", IP: "10.115.0.3"}}); err != nil {
		t.Fatal(err)
	}
	got := readCurrent(t, p, "domains.cfg")
	if strings.Contains(got, "a.example.com") {
		t.Fatalf("previous generation's domain survived:\n%s", got)
	}
	if !strings.Contains(got, "b.example.com") {
		t.Fatalf("new domain missing:\n%s", got)
	}
	if p.Generation() != 2 {
		t.Fatalf("generation = %d, want 2", p.Generation())
	}
}

// A failed validation must NOT move "current": the previously published
// generation has to keep serving traffic.
func TestValidationFailureKeepsCurrent(t *testing.T) {
	p := newTestProxy(t)
	if err := p.PublishAll([]Domain{{Domain: "good.example.com", IP: "10.115.0.2"}}); err != nil {
		t.Fatal(err)
	}
	genBefore := p.Generation()

	p.SetCheck(func(string) error { return os.ErrInvalid })
	if err := p.PublishAll([]Domain{{Domain: "bad.example.com", IP: "10.115.0.3"}}); err == nil {
		t.Fatal("publish should fail when the configuration check fails")
	}
	if p.Generation() != genBefore {
		t.Fatalf("generation moved to %d after a failed check", p.Generation())
	}
	got := readCurrent(t, p, "domains.cfg")
	if !strings.Contains(got, "good.example.com") || strings.Contains(got, "bad.example.com") {
		t.Fatalf("failed generation leaked into current:\n%s", got)
	}
	// The rejected release directory must not be left behind.
	ents, err := os.ReadDir(filepath.Join(p.Dir(), "releases"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() != filepath.Base(mustReadlink(t, filepath.Join(p.Dir(), "current"))) {
			t.Fatalf("stale release %s left behind", e.Name())
		}
	}
}

func mustReadlink(t *testing.T, path string) string {
	t.Helper()
	target, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(filepath.Dir(path), target)
}

// Publishing an empty set is valid: every domain was unbound, so the proxy
// keeps running with only the reject backends.
func TestPublishEmptySet(t *testing.T) {
	p := newTestProxy(t)
	if err := p.PublishAll(nil); err != nil {
		t.Fatal(err)
	}
	// No backends and, crucially, empty maps: every request falls through to
	// the reject backends, which is the correct state before the first domain.
	if got := readCurrent(t, p, "domains.cfg"); strings.Contains(got, "backend be_h_") || strings.Contains(got, "backend be_t_") {
		t.Fatalf("empty set still declares backends:\n%s", got)
	}
	for _, name := range []string{"maps/http.map", "maps/sni.map"} {
		if got := readCurrent(t, p, name); strings.Contains(got, "be_") {
			t.Fatalf("%s still routes domains:\n%s", name, got)
		}
	}
}

// The entry config's include target must exist for the generation HAProxy is
// asked to load, and the manifest must record the domain count.
func TestManifestAndLayout(t *testing.T) {
	p := newTestProxy(t)
	if err := p.PublishAll([]Domain{{Domain: "example.com", IP: "10.115.0.2"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.Dir(), "current", "manifest")); err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(p.Dir(), "generation"))
	if err != nil {
		t.Fatalf("generation counter missing: %v", err)
	}
	if strings.TrimSpace(string(b)) != "1" {
		t.Fatalf("generation counter = %q", b)
	}
}

// Generations are pruned, but the live one is never removed.
func TestPruneKeepsCurrentGeneration(t *testing.T) {
	p := newTestProxy(t)
	for i := 0; i < 6; i++ {
		if err := p.PublishAll([]Domain{{Domain: "example.com", IP: "10.115.0.2"}}); err != nil {
			t.Fatal(err)
		}
	}
	ents, err := os.ReadDir(filepath.Join(p.Dir(), "releases"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) > keepReleases {
		t.Fatalf("kept %d releases, want at most %d", len(ents), keepReleases)
	}
	if _, err := os.Stat(filepath.Join(p.Dir(), "current", "domains.cfg")); err != nil {
		t.Fatalf("current generation was pruned: %v", err)
	}
}
