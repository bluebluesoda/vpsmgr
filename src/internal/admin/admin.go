package admin

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

// Server is the admin panel: a separate secret path served by the same TLS
// listener as the user panel. Requests that hit neither secret path get the
// featureless 404 from the dispatcher in main.
type Server struct {
	cfg      *cfg.Config
	db       *db.DB
	mgr      *mgr.Manager
	sessions *sessionStore
	limiter  *loginLimiter
	flash    *flashStore
}

// New builds the admin server. maxSessions bounds the in-memory session map.
func New(c *cfg.Config, d *db.DB, m *mgr.Manager) *Server {
	return &Server{
		cfg:      c,
		db:       d,
		mgr:      m,
		sessions: newSessionStore(2048),
		limiter:  newLoginLimiter(),
		flash:    newFlashStore(),
	}
}

// prefix returns the secret path prefix of the admin panel.
func (s *Server) prefix() string { return "/" + s.cfg.Panel.AdminPath }

// p joins the admin prefix with a route.
func (s *Server) p(route string) string { return s.prefix() + route }

func (s *Server) templates() (*template.Template, error) {
	return template.ParseFS(tmplFS, "templates/*.html")
}

// Handler returns the admin panel handler. The caller (main) dispatches
// requests to this handler only when the URL path is under the admin prefix;
// everything else is handled elsewhere or gets a featureless 404.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.requireAuth(s.requirePost(s.handleLogout)))
	mux.HandleFunc("/", s.requireAuth(s.handleOverview))
	mux.HandleFunc("/domains", s.requireAuth(s.handleDomains))
	mux.HandleFunc("/domain-del", s.requireAuth(s.requirePost(s.handleDomainDel)))
	mux.HandleFunc("/domain-update", s.requireAuth(s.requirePost(s.handleDomainUpdate)))
	mux.HandleFunc("/audit", s.requireAuth(s.handleAudit))
	mux.HandleFunc("/audit/api", s.requireAuth(s.handleAuditAPI))
	mux.HandleFunc("/user-add", s.requireAuth(s.requirePost(s.handleUserAdd)))
	mux.HandleFunc("/user-del", s.requireAuth(s.requirePost(s.handleUserDel)))
	mux.HandleFunc("/user-quota", s.requireAuth(s.requirePost(s.handleUserQuota)))
	mux.HandleFunc("/power", s.requireAuth(s.requirePost(s.handlePower)))
	mux.HandleFunc("/reset-panel-pass", s.requireAuth(s.requirePost(s.handleResetPanelPass)))
	mux.HandleFunc("/admin-pass", s.requireAuth(s.requirePost(s.handleAdminPass)))
	mux.HandleFunc("/flash", s.requireAuth(s.requirePost(s.handleFlash)))
	prefix := s.prefix()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rest, ok := stripPrefix(r.URL.Path, prefix)
		if !ok {
			// The dispatcher already guarantees the prefix, but keep the
			// invariant: never serve admin content off-prefix.
			w.WriteHeader(http.StatusNotFound)
			return
		}
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
		// Resolve the admin language once per request and persist an explicit
		// ?lang= choice in a scoped cookie so it survives page navigations
		// (same behavior as the user panel).
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

// stripPrefix removes the admin prefix from path.
func stripPrefix(path, prefix string) (string, bool) {
	if path == prefix {
		return "/", true
	}
	if strings.HasPrefix(path, prefix+"/") {
		return strings.TrimPrefix(path, prefix), true
	}
	return "", false
}

// loginLimiter is a per-IP rate limiter for the admin login (same policy as
// the user panel: 5 attempts / 60s).
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
	return w.count <= l.limit
}

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

// requireAuth gates admin routes on a valid admin session cookie.
func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("vpsmgr_admin_session")
		if err != nil || !s.sessions.valid(c.Value) {
			s.clearSessionCookie(w)
			s.redirect(w, r, s.p("/login"), "")
			return
		}
		next(w, r)
	}
}

// requirePost rejects everything but POST so no state-changing action can be
// triggered by a top-level GET navigation. A wrong method gets the same bare
// 404 as any other wrong path — never a 405, which would advertise that a
// POST-only endpoint exists here.
//
// It also caps the request body at 1 MiB (all admin forms are small), so
// oversized submissions are rejected before ParseForm allocates anything.
func (s *Server) requirePost(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		next(w, r)
	}
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "vpsmgr_admin_session",
		Value:    token,
		Path:     s.prefix(),
		MaxAge:   s.cfg.Panel.SessionDays * 86400,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Server) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     "vpsmgr_admin_session",
		Value:    "",
		MaxAge:   -1,
		Path:     s.prefix(),
		HttpOnly: true,
		Secure:   true,
	})
}

func (s *Server) redirect(w http.ResponseWriter, r *http.Request, path, msg string) {
	s.storeFlash(r, msg, "toast")
	http.Redirect(w, r, path, http.StatusFound)
}

func (s *Server) redirectModal(w http.ResponseWriter, r *http.Request, path, msg string) {
	s.storeFlash(r, msg, "modal")
	http.Redirect(w, r, path, http.StatusFound)
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
