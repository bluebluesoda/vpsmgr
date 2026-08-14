package tfx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"vpsmgr/internal/cfg"
)

type Traefik struct {
	dir string
}

func New(c *cfg.Config) *Traefik { return &Traefik{dir: c.TraefikDir()} }

// sanitizeDomain maps a normalized domain to a traefik element name that is
// collision-free: dots become underscores, hyphens and alphanumerics stay.
// Because domains only contain [a-z0-9.-], the mapping is injective, so two
// different domains can never produce the same router/service name (e.g.
// example.com -> example_com versus example-com -> example-com). Traefik
// element names must be globally unique across all dynamic files.
func sanitizeDomain(d string) string {
	var b strings.Builder
	for _, r := range d {
		if r == '.' {
			b.WriteByte('_')
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// filePath is the per-domain dynamic file: <domain>.yaml. Domain names are
// normalized to [a-z0-9.-], so this is a safe filename (no slashes, no dots
// colliding with the extension logic).
func (t *Traefik) filePath(domain string) string {
	return filepath.Join(t.dir, domain+".yaml")
}

// WriteDomain writes one domain's complete dynamic config: an HTTP router +
// service (port 80) and a TLS-passthrough TCP router + service (port 443).
// When proxyProtocol is enabled the TCP service sends PROXY protocol v2 to the
// container before the TLS bytes (the backend must support it; HTTP/80 has no
// proxy protocol - traefik injects X-Forwarded-For headers there). The direct
// `loadBalancer.proxyProtocol` form matches traefik 3.3.5 (the pinned version);
// traefik >= 3.5.2 deprecated it in favour of a `serversTransport` - revisit
// when upgrading. Atomic: written to a temp file and renamed.
func (t *Traefik) WriteDomain(domain, ip string, proxyProtocol bool) error {
	san := sanitizeDomain(domain)
	var b strings.Builder
	b.WriteString(cfg.GeneratedBanner)
	fmt.Fprintf(&b, "http:\n  routers:\n    u-%s:\n      rule: \"Host(`%s`)\"\n      entryPoints: [web]\n      service: s-%s\n", san, domain, san)
	fmt.Fprintf(&b, "  services:\n    s-%s:\n      loadBalancer:\n        servers: [{ url: \"http://%s:80\" }]\n", san, ip)
	fmt.Fprintf(&b, "tcp:\n  routers:\n    t-%s:\n      rule: \"HostSNI(`%s`)\"\n      entryPoints: [websecure]\n      tls: { passthrough: true }\n      service: t-%s\n", san, domain, san)
	fmt.Fprintf(&b, "  services:\n    t-%s:\n      loadBalancer:\n        servers: [{ address: \"%s:443\" }]\n", san, ip)
	if proxyProtocol {
		fmt.Fprintf(&b, "        proxyProtocol:\n          version: 2\n")
	}
	tmp := t.filePath(domain) + fmt.Sprintf(".tmp.%d", os.Getpid())
	if err := os.WriteFile(tmp, []byte(b.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, t.filePath(domain))
}

// RemoveDomain removes a domain's dynamic file. Already-gone files are treated
// as success.
func (t *Traefik) RemoveDomain(domain string) error {
	err := os.Remove(t.filePath(domain))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ListFiles returns the base domain names of every *.yaml file in the dynamic
// directory (the part before ".yaml"), for orphan cleanup during
// reconciliation.
func (t *Traefik) ListFiles() ([]string, error) {
	ents, err := os.ReadDir(t.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if name := e.Name(); strings.HasSuffix(name, ".yaml") {
			out = append(out, strings.TrimSuffix(name, ".yaml"))
		}
	}
	return out, nil
}
