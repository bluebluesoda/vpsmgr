package panel

import (
	"embed"
	"html/template"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/csrf"
	"vpsmgr/internal/db"
	"vpsmgr/internal/mgr"
)

//go:embed templates/*.html
var tmplFS embed.FS

const (
	loginLimit    = 5
	loginWindow   = 60 * time.Second
	limiterMaxIPs = 10000
)

type loginRecord struct {
	start time.Time
	count int
}

type loginLimiter struct {
	mu    sync.Mutex
	byIP  map[string]*loginRecord
	limit int
	win   time.Duration
}

func newLoginLimiter() *loginLimiter {
	return &loginLimiter{byIP: make(map[string]*loginRecord), limit: loginLimit, win: loginWindow}
}

// allowed increments the attempt counter for ip and reports whether the
// attempt may proceed (limit is attempts per window per IP).
func (l *loginLimiter) allowed(ip string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	w, ok := l.byIP[ip]
	if !ok || now.Sub(w.start) >= l.win {
		l.byIP[ip] = &loginRecord{start: now, count: 1}
		return true
	}
	w.count++
	if w.count > l.limit {
		return false
	}
	return true
}

// prune removes stale entries to bound memory.
func (l *loginLimiter) prune() {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.byIP) < limiterMaxIPs {
		return
	}
	for ip, w := range l.byIP {
		if now.Sub(w.start) >= l.win {
			delete(l.byIP, ip)
		}
	}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

type Server struct {
	cfg     *cfg.Config
	db      *db.DB
	mgr     *mgr.Manager
	limiter *loginLimiter
	flash   *flashStore
}

func New(c *cfg.Config, d *db.DB, m *mgr.Manager) *Server {
	return &Server{cfg: c, db: d, mgr: m, limiter: newLoginLimiter(), flash: newFlashStore()}
}

func (s *Server) templates() (*template.Template, error) {
	return template.ParseFS(tmplFS, "templates/*.html")
}

// prefix returns the secret path prefix every panel route lives under.
func (s *Server) prefix() string { return "/" + s.cfg.Panel.URLPath }

// p joins the prefix with a panel route (e.g. p("/login")).
func (s *Server) p(route string) string { return s.prefix() + route }

// domainRow is one domain in the user's overview list, with its PROXY
// protocol toggle state.
type domainRow struct {
	Domain        string
	ProxyProtocol bool
}

type pageData struct {
	Title          string
	User           *db.User
	State          string
	IP             string
	SSHPort        int
	StartPort      int
	Ports          string // full user-port block, e.g. 10700-10799 (tooltip)
	PortsShort     string // compact form, e.g. 107xx
	SSH            string
	V4Forward      bool   // false = IPv6-only box: v4 ssh/ports not offered
	InitScript     string // custom init script, run after a reinstall
	BandwidthQuotaGB int    // monthly bandwidth quota GiB, 0 = unlimited
	BandwidthUsedGB  string // used this month (GB, 1 decimal) — only set when limited
	BandwidthPct     int    // used/quota * 100, clamped to 100
	Throttled      bool   // over quota: NIC limited to 1Mbps
	Domains        []domainRow
	QuotaCPU       string
	QuotaMem       string
	QuotaDisk      string
	Msg            string
	Err            string
	PublicIP       string
	Prefix         string
	Lang           string
	UpGB           string
	DownGB         string
	IPv6           string // primary global address (the one to connect to)
	IPv6Block      string // the /112 block the container owns (informational)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.requireAuth(s.requirePost(s.handleLogout)))
	mux.HandleFunc("/", s.requireAuth(s.handleOverview))
	mux.HandleFunc("/power", s.requireAuth(s.requirePost(s.handlePower)))
	mux.HandleFunc("/reinstall", s.requireAuth(s.requirePost(s.handleReinstall)))
	mux.HandleFunc("/password", s.requireAuth(s.requirePost(s.handlePanelPassword)))
	mux.HandleFunc("/root-reset", s.requireAuth(s.requirePost(s.handleRootReset)))
	mux.HandleFunc("/domain-add", s.requireAuth(s.requirePost(s.handleDomainAdd)))
	mux.HandleFunc("/domain-del", s.requireAuth(s.requirePost(s.handleDomainDel)))
	mux.HandleFunc("/domain-update", s.requireAuth(s.requirePost(s.handleDomainUpdate)))
	mux.HandleFunc("/init-script", s.requireAuth(s.requirePost(s.handleInitScript)))
	mux.HandleFunc("/images", s.requireAuth(s.requirePost(s.handleImages)))
	mux.HandleFunc("/flash", s.requireAuth(s.requirePost(s.handleFlash)))
	prefix := s.prefix()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := stripPrefix(r.URL.Path, prefix)
		if !ok {
			// Never reach the mux: scanners probing random paths get a bare
			// 404 with no fingerprint and no auth/rate-limit cost. No headers
			// are set here on purpose — any header would fingerprint the service.
			featureless404(w)
			return
		}
		// Security headers apply only to real panel responses behind the prefix,
		// never to the featureless 404 above.
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy",
			"default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'self'")
		// CSRF: reject cross-origin POSTs before any handler runs. The session
		// cookie is SameSite=Lax (no cookies on cross-site POSTs); this check
		// additionally stops login CSRF (which needs no session) and
		// same-site-subdomain requests.
		if r.Method == http.MethodPost && !csrf.Allowed(r) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		// Resolve the panel language once per request and persist an explicit
		// ?lang= choice in a scoped cookie so it survives page navigations.
		l := s.lang(r)
		if l != "" && r.URL.Query().Get("lang") != "" {
			http.SetCookie(w, &http.Cookie{
				Name:     langCookie,
				Value:    l,
				Path:     prefix,
				MaxAge:   365 * 24 * 3600,
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = rest
		r2 = r2.WithContext(withLang(r2.Context(), l))
		mux.ServeHTTP(w, r2)
	})
}

// stripPrefix removes the secret prefix from path, returning the path under
// the prefix. ok is false when path is not below the prefix.
func stripPrefix(path, prefix string) (string, bool) {
	if path == prefix {
		return "/", true
	}
	if strings.HasPrefix(path, prefix+"/") {
		return strings.TrimPrefix(path, prefix), true
	}
	return "", false
}

// featureless404 replies with a bare 404: empty body, no Content-Type, so all
// wrong paths look identical and reveal nothing about the server.
func featureless404(w http.ResponseWriter) {
	w.WriteHeader(http.StatusNotFound)
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, data pageData) {
	s.renderStatus(w, r, http.StatusOK, name, data)
}

func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, status int, name string, data pageData) {
	t, err := s.templates()
	if err != nil {
		http.Error(w, "template error: "+err.Error(), 500)
		return
	}
	// Language is resolved once per request in the top-level handler; fall
	// back to a direct detection for direct template execution (tests).
	if data.Lang == "" {
		data.Lang = langEn
		if l, ok := r.Context().Value(langCtxKey).(string); ok && l != "" {
			data.Lang = l
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := t.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request, path, msg string) {
	s.storeFlash(r, msg, "toast")
	http.Redirect(w, r, path, http.StatusFound)
}

// redirectModal is like redirect but the banner is shown as a modal (used for
// one-time secrets such as freshly generated passwords).
func (s *Server) redirectModal(w http.ResponseWriter, r *http.Request, path, msg string) {
	s.storeFlash(r, msg, "modal")
	http.Redirect(w, r, path, http.StatusFound)
}

func (s *Server) buildData(u *db.User, msg, errMsg string) pageData {
	d := pageData{
		Title:      "VPS Manager",
		User:       u,
		PublicIP:   s.cfg.DisplayIP(),
		Prefix:     s.prefix(),
		SSHPort:    u.SSHPort,
		StartPort:  u.StartPort,
		Ports:      mgr.UserPorts(u.StartPort, cfg.PortsPerUser),
		PortsShort: mgr.UserPortsShort(u.StartPort),
		SSH:        "ssh -p " + itoa(u.SSHPort) + " root@" + s.cfg.DisplayIP(),
		V4Forward:  s.mgr.V4ForwardLive(),
		InitScript: u.InitScript,
		QuotaCPU:   mgr.FormatCPU(u.CPU),
		QuotaMem:   itoa(u.MemMB) + " MiB",
		QuotaDisk:  itoa(u.DiskGB) + " GiB",
		Msg:        msg,
		Err:        errMsg,
	}
	// One `incus list` call only for the container status (must be live).
	// Bandwidth is read from the DB — the background sampler writes it every 60s.
	st, err := s.mgr.State(u.Name)
	if err != nil {
		d.Err = err.Error()
	} else if st != "" {
		d.State = st
		d.IP = u.IP
	}
	if s.cfg.IPv6Enabled() {
		if ipv6, _ := s.mgr.IPv6Addr(u.Name); ipv6 != "" { // pure computation, no incus call
			d.IPv6 = ipv6
		}
		if b, _ := s.mgr.IPv6Block(u.Name); b != nil {
			d.IPv6Block = b.String()
		}
	}
	up, down := s.mgr.BandwidthFor(u.ID) // pure DB read
	d.UpGB = mgr.FormatGB(up)
	d.DownGB = mgr.FormatGB(down)
	if q := u.BandwidthQuotaGB; q > 0 {
		used := up + down
		quota := uint64(q) << 30
		pct := int(used * 100 / quota)
		if pct > 100 {
			pct = 100
		}
		d.BandwidthQuotaGB = q
		d.BandwidthUsedGB = mgr.FormatGB(used)
		d.BandwidthPct = pct
		d.Throttled = s.mgr.IsThrottled(u.Name)
	}
	domains, _ := s.db.ListDomains(u.ID)
	for _, x := range domains {
		d.Domains = append(d.Domains, domainRow{Domain: x.Domain, ProxyProtocol: x.ProxyProtocol})
	}
	return d
}
