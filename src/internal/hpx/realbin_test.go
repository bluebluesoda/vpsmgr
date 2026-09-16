package hpx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vpsmgr/internal/cfg"
)

// TestGeneratedConfigIsAcceptedByRealHAProxy runs the configuration the panel
// actually renders (main config + per-domain backends and routing rules)
// through a REAL `haproxy -c`, and then STARTS it to prove haproxy not only
// parses the file but binds and runs with it.
//
// Skipped unless a real binary is provided, so the default `go test ./...`
// stays hermetic:
//
//	VPSMGR_TEST_HAPROXY_BIN=/usr/sbin/haproxy CGO_ENABLED=0 \
//	  go test ./internal/hpx/ -run RealHAProxy -v
func TestGeneratedConfigIsAcceptedByRealHAProxy(t *testing.T) {
	bin := os.Getenv("VPSMGR_TEST_HAPROXY_BIN")
	if bin == "" {
		t.Skip("VPSMGR_TEST_HAPROXY_BIN not set; skipping the real-binary validation")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("VPSMGR_TEST_HAPROXY_BIN=%s: %v", bin, err)
	}

	dir := t.TempDir()
	t.Setenv("VPSMGR_HAPROXY_DIR", dir)
	t.Setenv("VPSMGR_HAPROXY_BIN", bin)

	p := New(cfg.Default())
	// The installed entry config is not in the temp layout, and it is loaded
	// FIRST by the service; render it here so the check resolves every map.
	if err := p.WriteEntryConfig(); err != nil {
		t.Fatal(err)
	}
	// A realistic mix: several domains, one with PROXY protocol enabled, a
	// hyphenated label and a subdomain, plus a long one.
	domains := []Domain{
		{Domain: "example.com", IP: "10.115.0.2"},
		{Domain: "api.example.com", IP: "10.115.0.3", ProxyProtocol: true},
		{Domain: "my-site.example.com", IP: "10.115.0.4"},
		{Domain: "a-very-long-subdomain-label.example.com", IP: "10.115.0.5", ProxyProtocol: true},
	}
	// PublishAll calls validate() itself when no stub is installed — that is
	// exactly the real `haproxy -c -f <generation>` we want to exercise.
	if err := p.PublishAll(domains); err != nil {
		t.Fatalf("real haproxy rejected the generated configuration: %v", err)
	}
	domainsCfg := filepath.Join(dir, "current", "domains.cfg")
	t.Logf("haproxy accepted generation %d (%s)", p.Generation(), domainsCfg)

	// Start it for real. The template binds :80/:443, which a busy test host
	// may already own, so run it in a throwaway network namespace: there it is
	// the only listener, and binding privileged ports as root works. When the
	// namespace cannot be created the parse result above still stands.
	if out, err := exec.Command("unshare", "-n", "true").CombinedOutput(); err != nil {
		t.Skipf("cannot create a network namespace (%v: %s); skipping the start test", err, strings.TrimSpace(string(out)))
	}
	cmd := exec.Command("unshare", "-n", bin, "-f", p.entryPath(), "-f", domainsCfg)
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatalf("could not start haproxy: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		t.Fatalf("haproxy exited immediately — the generated configuration is not loadable: %v", err)
	case <-time.After(1500 * time.Millisecond):
		// Still running with no bind error: a live process accepted it.
	}
}

// TestBootstrapConfigIsAcceptedByRealHAProxy validates the vendored bootstrap
// files that scripts/30-haproxy.sh installs before the panel ever runs: the
// static entry config, which includes an empty first generation.
func TestBootstrapConfigIsAcceptedByRealHAProxy(t *testing.T) {
	bin := os.Getenv("VPSMGR_TEST_HAPROXY_BIN")
	if bin == "" {
		t.Skip("VPSMGR_TEST_HAPROXY_BIN not set")
	}
	// The repository's configs live three levels above src/.
	root := filepath.Join("..", "..", "..", "configs", "haproxy")
	entry, err := filepath.Abs(filepath.Join(root, "haproxy.cfg"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(entry); err != nil {
		t.Skipf("vendored config not found at %s: %v", entry, err)
	}

	// Reproduce the installed layout in a temp dir: the entry config includes
	// an ABSOLUTE path to the generation, so point that at the temp copy.
	dir := t.TempDir()
	gen := filepath.Join(dir, "releases", "000001")
	if err := os.MkdirAll(gen, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(gen, "maps"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"domains.cfg", filepath.Join("maps", "http.map"), filepath.Join("maps", "sni.map")} {
		b, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(gen, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join("releases", "000001"), filepath.Join(dir, "current")); err != nil {
		t.Fatal(err)
	}

	// The entry config is installed at the layout root; render it for this
	// temp layout (same content, paths redirected).
	p := &Proxy{dir: dir, bin: bin}
	if err := p.WriteEntryConfig(); err != nil {
		t.Fatal(err)
	}
	if err := p.validate(gen); err != nil {
		t.Fatalf("real haproxy rejected the vendored bootstrap configuration: %v", err)
	}

	// The vendored bootstrap and the renderer must agree, name for name.
	vendored, err := os.ReadFile(entry)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"fe_http", "fe_tls", "be_http_reject", "be_tls_reject", "inspect-delay"} {
		if !strings.Contains(string(vendored), want) {
			t.Errorf("vendored haproxy.cfg is missing %q", want)
		}
	}
}

// TestMainConfigTemplateMatchesBootstrap guards the one thing a duplicated
// template can break silently: the renderer and the vendored bootstrap copy
// must declare the same proxy sections, or a published generation would not
// line up with the frontends and reject backends the entry config expects.
func TestMainConfigTemplateMatchesBootstrap(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "configs", "haproxy", "proxy.cfg"))
	if err != nil {
		t.Skipf("vendored bootstrap config not readable: %v", err)
	}
	// Compare the meaningful lines only: the comments differ between the two
	// copies on purpose (one explains the host layout, the other the panel).
	norm := func(s string) string {
		var out []string
		for _, line := range strings.Split(s, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			out = append(out, line)
		}
		return strings.Join(out, "\n")
	}
	want := norm(renderMainConfig())
	got := norm(string(b))
	if want != got {
		t.Errorf("configs/haproxy/proxy.cfg has drifted from renderMainConfig():\n--- renderer ---\n%s\n--- bootstrap ---\n%s", want, got)
	}
}
