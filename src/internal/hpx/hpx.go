// Package hpx renders and publishes the HAProxy configuration that fronts
// every user's domain.
//
// The panel never edits the live configuration in place. Instead it builds a
// complete, self-contained "generation" directory, validates it with the
// pinned HAProxy binary, and only then atomically repoints a symlink at it.
// A rejected generation leaves the running proxy untouched.
//
// Layout rooted at cfg.Config.ProxyDir() (default /etc/haproxy):
//
//	haproxy.cfg          static entry config (installed once, never rewritten)
//	releases/<gen>/      one immutable generation
//	releases/<gen>/domains.cfg    one backend per domain
//	releases/<gen>/maps/http.map  Host -> backend  (port 80)
//	releases/<gen>/maps/sni.map   SNI  -> backend  (port 443)
//	releases/<gen>/manifest       provenance (generation, domain count, time)
//	current -> releases/<gen>     the generation HAProxy actually loads
//	generation           monotonic counter of the last published generation
//
// The service loads the entry config first and the current generation second
// (two -f arguments), which is why the frontends live in the entry config and
// the per-domain backends in the generation.
//
// A release is published in three steps: write the new release directory,
// fsync, then atomically replace the "current" symlink. HAProxy only ever
// reads through the symlink, and a reload re-reads it, so an interrupted
// publish leaves the previous generation serving traffic.
package hpx

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"vpsmgr/internal/cfg"
)

// Proxy is the HAProxy configuration publisher.
type Proxy struct {
	dir string
	// bin is the HAProxy binary used for the configuration check. It is always
	// an absolute path so the check can never pick up a different build from
	// PATH (see cfg.HaproxyBin).
	bin string
	// check overrides the validation step (tests inject a stub). nil = run
	// bin -c on the candidate release.
	check func(releaseCfg string) error
}

// Domain is one published mapping: a normalized domain routed to a container
// IPv4. ProxyProtocol enables PROXY protocol v2 towards the 443 backend only.
type Domain struct {
	Domain        string
	IP            string
	ProxyProtocol bool
}

// New returns a publisher for the configured proxy directory.
//
// VPSMGR_HAPROXY_BIN overrides the binary used for the configuration check
// (tests substitute a stub; the real one is /usr/sbin/haproxy).
func New(c *cfg.Config) *Proxy {
	bin := cfg.DefaultHaproxyBin
	if p := os.Getenv("VPSMGR_HAPROXY_BIN"); p != "" {
		bin = p
	}
	return &Proxy{dir: c.ProxyDir(), bin: bin}
}

// SetCheck overrides the configuration-validation step. Tests only.
func (p *Proxy) SetCheck(f func(releaseCfg string) error) { p.check = f }

// Dir reports the layout root (tests and diagnostics).
func (p *Proxy) Dir() string { return p.dir }

func (p *Proxy) entryPath() string      { return filepath.Join(p.dir, "haproxy.cfg") }
func (p *Proxy) releasesDir() string    { return filepath.Join(p.dir, "releases") }
func (p *Proxy) currentLink() string    { return filepath.Join(p.dir, "current") }
func (p *Proxy) generationPath() string { return filepath.Join(p.dir, "generation") }

func (p *Proxy) releaseDir(gen int) string {
	return filepath.Join(p.releasesDir(), fmt.Sprintf("%06d", gen))
}

// sanitizeDomain maps a normalized domain to a collision-free identifier
// usable in HAProxy object names: dots become underscores, hyphens and
// alphanumerics stay. Domains are restricted to [a-z0-9.-] before they reach
// this layer, so the mapping is injective and two different domains can never
// produce the same backend name (example.com -> example_com versus
// example-com -> example-com).
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

// domainID is the stable per-domain suffix used in backend names. A short hash
// keeps names unique even if two domains sanitize differently but collide by
// case (defensive: the domain set is already validated and unique).
func domainID(domain string) string {
	sum := sha256.Sum256([]byte(domain))
	return hex.EncodeToString(sum[:4])
}

func httpBackendName(domain string) string { return "be_h_" + sanitizeDomain(domain) }
func tlsBackendName(domain string) string  { return "be_t_" + sanitizeDomain(domain) }

// WriteDomain publishes a single-domain set. It exists for callers that have
// exactly one domain (and for tests); the manager always republishes the full
// set, because a generation must describe every domain at once.
func (p *Proxy) WriteDomain(domain, ip string, proxyProtocol bool) error {
	return p.PublishAll([]Domain{{Domain: domain, IP: ip, ProxyProtocol: proxyProtocol}})
}

// PublishAll renders every domain into a fresh generation, validates it, and
// atomically switches the "current" symlink to it.
//
// Callers pass the COMPLETE desired set (from the database). Rendering the
// whole set every time is what makes the result independent of any previous
// state: a stale file can never survive into a later generation.
func (p *Proxy) PublishAll(domains []Domain) error {
	if err := os.MkdirAll(p.releasesDir(), 0o755); err != nil {
		return fmt.Errorf("hpx: create releases dir: %w", err)
	}
	gen := p.nextGeneration()
	dir := p.releaseDir(gen)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("hpx: create release dir: %w", err)
	}

	// Any failure below must not leave a half-written release behind that a
	// future generation number could reuse in a confusing way; the directory is
	// uniquely numbered, so simply removing it is safe.
	ok := false
	defer func() {
		if !ok {
			// The generation never became valid; drop its directory but leave
			// the "current" link exactly as it was (validateRelease already
			// restored it on failure).
			os.RemoveAll(dir)
		}
	}()

	domainsPath := filepath.Join(dir, "domains.cfg")
	if err := writeFileSync(domainsPath, []byte(renderDomains(domains)), 0o644); err != nil {
		return fmt.Errorf("hpx: write domains: %w", err)
	}
	mapsDir := filepath.Join(dir, "maps")
	if err := os.MkdirAll(mapsDir, 0o755); err != nil {
		return fmt.Errorf("hpx: create maps dir: %w", err)
	}
	// The maps are what the frontends actually consult. They must be written
	// BEFORE validation, which loads them.
	if err := writeFileSync(filepath.Join(mapsDir, "http.map"), []byte(renderMap(domains, httpBackendName)), 0o644); err != nil {
		return fmt.Errorf("hpx: write http map: %w", err)
	}
	if err := writeFileSync(filepath.Join(mapsDir, "sni.map"), []byte(renderMap(domains, tlsBackendName)), 0o644); err != nil {
		return fmt.Errorf("hpx: write sni map: %w", err)
	}
	if err := writeFileSync(filepath.Join(dir, "manifest"), []byte(renderManifest(gen, domains)), 0o644); err != nil {
		return fmt.Errorf("hpx: write manifest: %w", err)
	}
	if err := fsyncDir(dir); err != nil {
		return fmt.Errorf("hpx: sync release dir: %w", err)
	}

	// Validate BEFORE the change is made permanent. The entry config resolves
	// its map paths through "current", so the candidate must be visible there
	// for the check to see the NEW maps rather than the old ones. The previous
	// target is kept and restored if the check fails, so a rejected generation
	// leaves the layout exactly as it was (and a running proxy, which already
	// loaded the old files, is unaffected either way).
	prev, hadPrev := p.currentTarget()
	if err := p.switchCurrent(dir); err != nil {
		return err
	}
	if err := p.validateRelease(dir); err != nil {
		if hadPrev {
			_ = p.switchCurrent(prev)
		} else {
			os.Remove(p.currentLink())
		}
		return err
	}
	if err := os.WriteFile(p.generationPath(), []byte(strconv.Itoa(gen)+"\n"), 0o644); err != nil {
		return fmt.Errorf("hpx: record generation: %w", err)
	}
	ok = true
	p.pruneOldReleases(gen)
	return nil
}

// validateRelease checks a release directory that "current" does not point at
// yet, by loading the static entry config together with that directory's own
// domains file — exactly the command line the service uses.
func (p *Proxy) validateRelease(dir string) error {
	return p.checkFiles(dir)
}

// validate checks a release directory (used by diagnostics and tests).
func (p *Proxy) validate(dir string) error {
	return p.checkFiles(dir)
}

// checkFiles runs the configuration check over the two files the service
// loads. The injected stub (tests, and any caller that cannot run a real
// binary) receives the generation's domains.cfg path.
func (p *Proxy) checkFiles(dir string) error {
	domainsCfg := filepath.Join(dir, "domains.cfg")
	if p.check != nil {
		return p.check(domainsCfg)
	}
	if _, err := os.Stat(p.bin); err != nil {
		return fmt.Errorf("hpx: proxy binary %s is missing: %w", p.bin, err)
	}
	out, err := exec.Command(p.bin, "-c", "-f", p.entryPath(), "-f", domainsCfg).CombinedOutput()
	if err != nil {
		return fmt.Errorf("hpx: proxy config check failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// WriteEntryConfig writes the layout's static entry config from the renderer's
// template, with the map paths pointing at THIS layout. Production installs
// the vendored copy (configs/haproxy/haproxy.cfg, identical content) with the
// host paths; tests call this so a real `haproxy -c` resolves every include.
func (p *Proxy) WriteEntryConfig() error {
	body := strings.ReplaceAll(renderMainConfig(), cfg.DefaultProxyDir, p.dir)
	return writeFileSync(p.entryPath(), []byte(body), 0o644)
}

// switchCurrent atomically repoints "current" at dir. The link is created
// under a temporary name and renamed, which is atomic on POSIX: a reader sees
// either the old or the new generation, never a missing link.
func (p *Proxy) switchCurrent(dir string) error {
	rel, err := filepath.Rel(p.dir, dir)
	if err != nil {
		return fmt.Errorf("hpx: relative release path: %w", err)
	}
	tmp := p.currentLink() + ".tmp"
	os.Remove(tmp)
	if err := os.Symlink(rel, tmp); err != nil {
		return fmt.Errorf("hpx: create current link: %w", err)
	}
	if err := os.Rename(tmp, p.currentLink()); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("hpx: switch current link: %w", err)
	}
	return fsyncDir(p.dir)
}

// nextGeneration returns the generation number to build: one past the recorded
// counter, or 1 when nothing has been published yet. The counter is only a
// monotonic label — correctness comes from building a complete release every
// time, so a lost counter can at worst reuse a directory name that prune()
// has already removed.
func (p *Proxy) nextGeneration() int {
	b, err := os.ReadFile(p.generationPath())
	if err != nil {
		return 1
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || n < 0 {
		return 1
	}
	return n + 1
}

// Generation reports the generation currently pointed at by "current" (0 when
// nothing has been published). Used for diagnostics and tests.
func (p *Proxy) Generation() int {
	prev, ok := p.currentTarget()
	if !ok {
		return 0
	}
	// The target is a zero-padded directory name ("releases/000004").
	n, err := strconv.Atoi(filepath.Base(prev))
	if err != nil {
		return 0
	}
	return n
}

// currentTarget resolves the "current" symlink to a release directory. ok is
// false when nothing has been published yet.
func (p *Proxy) currentTarget() (string, bool) {
	target, err := os.Readlink(p.currentLink())
	if err != nil {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(p.dir, target)
	}
	return target, true
}

// keepReleases is how many generations are retained for rollback/debugging.
// The running HAProxy keeps an open reference to files in its generation, so
// deleting the directory of a still-draining worker would only matter for
// files it has not read yet — maps and configs are read at load time, so a
// small retention window is enough.
const keepReleases = 3

func (p *Proxy) pruneOldReleases(current int) {
	ents, err := os.ReadDir(p.releasesDir())
	if err != nil {
		return
	}
	type rel struct {
		gen int
		dir string
	}
	var all []rel
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		n, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		all = append(all, rel{n, filepath.Join(p.releasesDir(), e.Name())})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].gen > all[j].gen })
	kept := 0
	for _, r := range all {
		if r.gen == current {
			kept++
			continue
		}
		kept++
		if kept > keepReleases {
			os.RemoveAll(r.dir)
		}
	}
}

// writeFileSync writes data to path and fsyncs the file, so the content is on
// disk before the generation is validated and published.
func writeFileSync(path string, data []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// fsyncDir flushes a directory entry so a rename/create is durable.
func fsyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// renderManifest records what the generation contains, so an operator can tell
// which set of domains a running process loaded.
func renderManifest(gen int, domains []Domain) string {
	var b strings.Builder
	fmt.Fprintf(&b, "generation: %d\n", gen)
	fmt.Fprintf(&b, "domains: %d\n", len(domains))
	fmt.Fprintf(&b, "created: %s\n", time.Now().UTC().Format(time.RFC3339))
	names := make([]string, 0, len(domains))
	for _, d := range domains {
		names = append(names, d.Domain)
	}
	sort.Strings(names)
	for _, n := range names {
		fmt.Fprintf(&b, "domain: %s\n", n)
	}
	return b.String()
}

// renderDomains emits the per-domain backends.
//
// Routing lives in the two MAP files of the same generation, not here, and the
// frontends live in the static entry config. That split is forced by HAProxy's
// parsing model:
//
//   - `use_backend` is a FRONTEND directive, and a bare directive belongs to
//     whichever proxy section was opened last. Emitting per-domain
//     `use_backend` lines from a file loaded after the entry config would
//     therefore attach them to the wrong proxy (and re-opening a frontend by
//     name is rejected: duplicate names are an error since HAProxy 3.3).
//   - expanding N domains into N frontend rules would also mean the frontends
//     change on every domain add/remove, i.e. a much larger reload surface.
//
// With maps the frontends stay constant and only two small files change:
//
//	host -> backend  (http.map)   SNI -> backend  (sni.map)
//
// A map miss yields the empty backend name, which HAProxy treats as "no rule
// matched" and falls through to the frontend's default_backend (the reject
// backend), so an unregistered host/SNI is never forwarded anywhere. An EMPTY
// map is valid too — that is the state before the first domain is added.
//
// This file therefore only declares the backends. They are reachable solely
// through the maps, which is why every one of them looks "unused" to a reader
// of the entry config.
func renderDomains(domains []Domain) string {
	var b bytes.Buffer
	b.WriteString(cfg.GeneratedBanner)
	b.WriteString("# One HTTP backend (container :80) and one TCP backend (container :443,\n")
	b.WriteString("# TLS passthrough) per published domain. The frontends select them through\n")
	b.WriteString("# the maps/http.map and maps/sni.map files of this same generation.\n\n")

	// Deterministic ordering keeps generations diffable.
	sorted := make([]Domain, len(domains))
	copy(sorted, domains)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Domain < sorted[j].Domain })

	for _, d := range sorted {
		hb := httpBackendName(d.Domain)
		tb := tlsBackendName(d.Domain)
		fmt.Fprintf(&b, "# ---- %s -> %s ----\n", d.Domain, d.IP)
		fmt.Fprintf(&b, "backend %s\n", hb)
		b.WriteString("    mode http\n")
		b.WriteString("    option forwardfor\n")
		fmt.Fprintf(&b, "    server s  %s:80 check\n\n", d.IP)

		fmt.Fprintf(&b, "backend %s\n", tb)
		b.WriteString("    mode tcp\n")
		if d.ProxyProtocol {
			// PROXY protocol v2 is a TCP-only feature and is deliberately NOT
			// sent on :80 (where X-Forwarded-For carries the client address).
			// The container's TLS listener must support it, or the handshake
			// fails — this mirrors the per-domain toggle the panel exposes.
			fmt.Fprintf(&b, "    server s  %s:443 check send-proxy-v2\n\n", d.IP)
		} else {
			fmt.Fprintf(&b, "    server s  %s:443 check\n\n", d.IP)
		}
	}
	return b.String()
}

// renderMap renders one "key backend" map file. Keys are the normalized
// domains; values are the backend names declared by renderDomains. The file is
// rewritten (never patched) with each generation, and HAProxy re-reads it on
// reload, because a map is a plain file loaded at startup.
func renderMap(domains []Domain, backend func(string) string) string {
	var b bytes.Buffer
	b.WriteString(cfg.GeneratedBanner)
	sorted := make([]Domain, len(domains))
	copy(sorted, domains)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Domain < sorted[j].Domain })
	for _, d := range sorted {
		fmt.Fprintf(&b, "%s %s\n", d.Domain, backend(d.Domain))
	}
	return b.String()
}

// renderMainConfig emits the STATIC entry configuration: it declares the
// frontends, the reject backends and the timeouts, and it is intentionally
// independent of the domain set. The installed copy lives at
// /etc/haproxy/haproxy.cfg and is never rewritten by the panel; the per-domain
// backends come from the generation, loaded with a second -f.
//
// Routing is expressed with MAP files so that adding or removing a domain
// never has to touch a frontend:
//
//	use_backend %[<key>,map(/etc/haproxy/current/maps/<name>.map)]
//
// A map hit yields the backend name; a miss yields an empty value, which
// HAProxy treats as "no rule matched" and falls through to default_backend
// (the reject backend). The map paths point INSIDE the current generation, so
// a graceful reload picks up the new ones.
//
// configs/haproxy/proxy.cfg holds the same content for the installer's
// bootstrap generation; TestMainConfigTemplateMatchesBootstrap keeps the two
// from drifting.
func renderMainConfig() string {
	return cfg.GeneratedBanner + `# HAProxy front for vpsmgr containers.
#
# :80  HTTP  -> container :80   (reverse proxy, X-Forwarded-For added)
# :443 TLS   -> container :443  (SNI routing, TLS PASSTHROUGH: HAProxy never
#                                terminates TLS, the container serves its own
#                                certificate)
#
# Every 80/443 listener is owned by this process alone; unknown hosts/SNI are
# answered by the reject backends below rather than forwarded anywhere.
#
# The per-domain backends live in the current generation, loaded by the service
# as a second -f (see /etc/systemd/system/haproxy.service).

global
    log /dev/log local0
    log /dev/log local1 notice
    maxconn 4096
    # Drop to the unprivileged account after binding 80/443 as root.
    user haproxy
    group haproxy
    # Long enough that a full drain of established connections is possible,
    # short enough that a stuck worker cannot accumulate across many reloads.
    hard-stop-after 5m
    stats socket /run/haproxy/admin.sock mode 660 level admin

defaults
    log global
    option dontlognull
    retries 2
    timeout connect 5s
    # Deliberately generous: SSH-style tunnels and WebSockets live for hours.
    timeout client 1h
    timeout server 1h
    timeout tunnel 1h

# ---------------------------------------------------------------------------
# :80 - HTTP host routing
# ---------------------------------------------------------------------------
frontend fe_http
    mode http
    bind :80
    option httplog
    # Normalize before the map lookup: lowercase, then drop a trailing ":port"
    # (req.hdr(host) keeps the port a client sent).
    http-request set-var(txn.host) req.hdr(host),lower,regsub(:[0-9]+$,)
    http-request set-header X-Forwarded-Proto http
    http-request set-header X-Forwarded-Port 80
    # The client address reaches the container as X-Forwarded-For, added by the
    # backends (option forwardfor). PROXY protocol is never used on :80.
    use_backend %[var(txn.host),map(/etc/haproxy/current/maps/http.map)]
    default_backend be_http_reject

backend be_http_reject
    mode http
    http-request deny deny_status 404

# ---------------------------------------------------------------------------
# :443 - TLS SNI routing (passthrough)
# ---------------------------------------------------------------------------
frontend fe_tls
    mode tcp
    bind :443
    option tcplog
    # Give the client a moment to send its ClientHello so the SNI can be read;
    # without this the SNI is not yet known when the backend is chosen.
    tcp-request inspect-delay 2s
    tcp-request content accept if { req.ssl_hello_type 1 }
    use_backend %[req.ssl_sni,lower,map(/etc/haproxy/current/maps/sni.map)]
    default_backend be_tls_reject

backend be_tls_reject
    mode tcp
    # No SNI, a host that is not registered, or anything that is not a TLS
    # ClientHello: close immediately instead of forwarding.
    tcp-request content reject
`
}
