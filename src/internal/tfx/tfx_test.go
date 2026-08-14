package tfx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vpsmgr/internal/cfg"
)

func TestSanitizeDomainCollisionFree(t *testing.T) {
	cases := map[string]string{
		"example.com":     "example_com",
		"example-com":     "example-com",
		"api.example.com": "api_example_com",
	}
	for in, want := range cases {
		if got := sanitizeDomain(in); got != want {
			t.Errorf("sanitizeDomain(%q) = %q, want %q", in, got, want)
		}
	}
	// example.com and example-com must never collide.
	if sanitizeDomain("example.com") == sanitizeDomain("example-com") {
		t.Fatal("example.com and example-com collide")
	}
}

// newTfx builds a Traefik pointing at a temp dir (via the env override).
func newTfx(t *testing.T) *Traefik {
	t.Helper()
	dir := t.TempDir()
	os.Setenv("VPSMGR_TRAEFIK_DIR", dir)
	t.Cleanup(func() { os.Unsetenv("VPSMGR_TRAEFIK_DIR") })
	return New(&cfg.Config{})
}

func TestWriteDomainYAML(t *testing.T) {
	tfx := newTfx(t)
	if err := tfx.WriteDomain("api.example.com", "10.115.0.2", true); err != nil {
		t.Fatal(err)
	}
	if err := tfx.WriteDomain("example.com", "10.115.0.2", false); err != nil {
		t.Fatal(err)
	}
	api, err := os.ReadFile(filepath.Join(tfx.dir, "api.example.com.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(api), "proxyProtocol:\n          version: 2") {
		t.Errorf("api yaml missing proxyProtocol:\n%s", api)
	}
	if !strings.Contains(string(api), "u-api_example_com") || !strings.Contains(string(api), "t-api_example_com") {
		t.Errorf("api yaml missing per-domain router/service names:\n%s", api)
	}
	plain, err := os.ReadFile(filepath.Join(tfx.dir, "example.com.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(plain), "proxyProtocol") {
		t.Errorf("plain domain must not have proxyProtocol:\n%s", plain)
	}
	// Remove + list.
	if err := tfx.RemoveDomain("example.com"); err != nil {
		t.Fatal(err)
	}
	files, err := tfx.ListFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0] != "api.example.com" {
		t.Errorf("ListFiles = %v", files)
	}
}
