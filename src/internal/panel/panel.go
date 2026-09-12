package panel

import (
	"embed"
	"html/template"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/csrf"
	"vpsmgr/internal/db"
	"vpsmgr/internal/mgr"
	"vpsmgr/internal/ver"
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

// snapshotRow is one container snapshot in the user's overview list.
type snapshotRow struct {
	Name      string
	CreatedAt string // UTC RFC3339; rendered in the browser's timezone
	Size      string // human-readable disk usage, e.g. "184 MiB"; may be empty
}

// sshKeyRow is one public key shown in the SSH-key management panel.
type sshKeyRow struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Key    string `json:"key"` // clean "type base64" (comment stripped)
	Active bool   `json:"active"`
}

// groupMemberRow is one container in the user's group switcher. Label and Specs
// are precomputed because the template cannot call mgr helpers or convert MiB.
type groupMemberRow struct {
	Name  string // account name, used as the <option> value
	Label string // "A" for the base account, else the numeric suffix
	Specs string // compact quota tag, e.g. "4c 8g 40g"
}

type pageData struct {
	Title              string
	User               *db.User
	GroupUsers         []groupMemberRow
	GroupIndex         string
	GroupCount         int
	State              string
	IP                 string
	SSHPort            int
	StartPort          int
	Ports              string // full user-port block, e.g. 10700-10799
	PortsPrefix        string // whole-hundred block number, e.g. 107 → "10700-10799"
	SSH                string
	V4Forward          bool   // false = IPv6-only box: v4 ssh/ports not offered
	TraefikEnabled     bool   // false = domain proxy disabled; domains cannot be added
	InitScript         string // custom init script, run after a reinstall
	BandwidthQuotaGB   int    // monthly bandwidth quota GiB, 0 = unlimited
	BandwidthUsedGB    string // used this month (GB, 1 decimal) — only set when limited
	BandwidthPct       int    // used/quota * 100, clamped to 100
	Throttled          bool   // over quota: NIC limited to 1Mbps
	ExpiresAt          string // quota validity deadline (RFC3339 UTC), "" = permanent
	Expired            bool   // deadline passed: account locked to read-only
	ExpiresSoon        bool   // within 72h of the deadline
	Domains            []domainRow
	QuotaCPU           string
	QuotaMem           string
	QuotaDisk          string
	CPUUse             string // 5-minute CPU average ("12%" or "-")
	MemUse             string // latest sampled memory usage ("345 MiB" or "-")
	DiskUsed           string // latest sampled disk usage ("184 MiB" or "-")
	Msg                string
	Err                string
	PublicIP           string
	Prefix             string
	Lang               string
	UpGB               string
	DownGB             string
	IPv6               string // primary global address (the one to connect to)
	IPv6Block          string // the /112 block the container owns (informational)
	Snapshots          []snapshotRow
	SnapshotLimit      int // configured per-container snapshot cap (for display)
	SSHKeys            []sshKeyRow
	AdminSSHKeys       []sshKeyRow // operator's own public keys, shown read-only
	AdminKeysAnyActive bool        // at least one admin key is granted: expand the disclosure by default
	Impersonated       bool        // operator "log in as user": show a banner
	AdminPrefix        string      // admin panel path, for the banner's return link
	MaxNotesPlaintext  int         // sticky-notes plaintext byte cap (client-side check)
	// ThemeColor is the accent color the operator assigned to this user ("" =
	// default). The overview tints its background and accents with it so an
	// impersonating admin can tell users apart. Users cannot set it themselves.
	ThemeColor string
	ShowFooter bool
	Version    string
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/logout", s.requireAuth(s.requirePost(s.handleLogout)))
	mux.HandleFunc("/switch-user", s.requireAuth(s.requirePost(s.handleSwitchUser)))
	mux.HandleFunc("/", s.requireAuth(s.handleOverview))
	mux.HandleFunc("/power", s.requireAuth(s.requireActive(s.requirePost(s.handlePower))))
	mux.HandleFunc("/reinstall", s.requireAuth(s.requireActive(s.requirePost(s.handleReinstall))))
	mux.HandleFunc("/password", s.requireAuth(s.requireActive(s.requirePost(s.handlePanelPassword))))
	mux.HandleFunc("/root-reset", s.requireAuth(s.requireActive(s.requirePost(s.handleRootReset))))
	mux.HandleFunc("/domain-add", s.requireAuth(s.requireActive(s.requirePost(s.handleDomainAdd))))
	mux.HandleFunc("/domain-del", s.requireAuth(s.requireActive(s.requirePost(s.handleDomainDel))))
	mux.HandleFunc("/domain-update", s.requireAuth(s.requireActive(s.requirePost(s.handleDomainUpdate))))
	mux.HandleFunc("/init-script", s.requireAuth(s.requireActive(s.requirePost(s.handleInitScript))))
	mux.HandleFunc("/stats", s.requireAuth(s.handleStats))
	mux.HandleFunc("/images", s.requireAuth(s.requirePost(s.handleImages)))
	mux.HandleFunc("/snapshot", s.requireAuth(s.requireActive(s.requirePost(s.handleSnapshot))))
	mux.HandleFunc("/snapshot-del", s.requireAuth(s.requireActive(s.requirePost(s.handleSnapshotDel))))
	mux.HandleFunc("/snapshot-restore", s.requireAuth(s.requireActive(s.requirePost(s.handleSnapshotRestore))))
	mux.HandleFunc("/ssh-keys", s.requireAuth(s.requireActive(s.requirePost(s.handleSSHKeys))))
	mux.HandleFunc("/notes", s.requireAuth(s.handleNotes))
	mux.HandleFunc("/notes/reset", s.requireAuth(s.requireActive(s.requirePost(s.handleNotesReset))))
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
	groupUsers, _ := s.mgr.UsersInGroup(u.Name)
	// Order: base account ("A") first, then children by numeric suffix, so the
	// switcher reads A, 1, 2, … 10 instead of the lexical A, 1, 10, 2.
	sort.Slice(groupUsers, func(i, j int) bool {
		gi, gj := mgr.ParseUserGroup(groupUsers[i].Name), mgr.ParseUserGroup(groupUsers[j].Name)
		if gi.Child != gj.Child {
			return !gi.Child
		}
		if gi.Child {
			ni, _ := strconv.Atoi(groupUsers[i].Name[len(gi.Parent)+1:])
			nj, _ := strconv.Atoi(groupUsers[j].Name[len(gj.Parent)+1:])
			if ni != nj {
				return ni < nj
			}
		}
		return groupUsers[i].Name < groupUsers[j].Name
	})
	groupRows := make([]groupMemberRow, 0, len(groupUsers))
	for _, m := range groupUsers {
		groupRows = append(groupRows, groupMemberRow{
			Name:  m.Name,
			Label: mgr.UserGroupLabel(m.Name),
			Specs: mgr.MachineSpecs(m.CPU, m.MemMB, m.DiskGB),
		})
	}
	groupIndex := mgr.UserGroupLabel(u.Name)
	d := pageData{
		Title:             "VPS Manager",
		User:              u,
		GroupUsers:        groupRows,
		GroupIndex:        groupIndex,
		GroupCount:        len(groupRows),
		ThemeColor:        u.Color,
		PublicIP:          s.cfg.DisplayIP(),
		Prefix:            s.prefix(),
		SSHPort:           u.SSHPort,
		StartPort:         u.StartPort,
		Ports:             mgr.UserPorts(u.StartPort, cfg.PortsPerUser),
		PortsPrefix:       itoa(u.StartPort / 100),
		SSH:               "ssh -p " + itoa(u.SSHPort) + " root@" + s.cfg.DisplayIP(),
		V4Forward:         s.mgr.V4ForwardLive(),
		TraefikEnabled:    s.mgr.TraefikLive(),
		ShowFooter:        s.cfg.Panel.ShowFooter,
		Version:           ver.Version,
		InitScript:        u.InitScript,
		MaxNotesPlaintext: cfg.MaxNotesPlaintextBytes,
		QuotaCPU:          mgr.FormatCPU(u.CPU),
		QuotaMem:          itoa(u.MemMB) + " MiB",
		QuotaDisk:         itoa(u.DiskGB) + " GiB",
		ExpiresAt:         u.ExpiresAt,
		Msg:               msg,
		Err:               errMsg,
	}
	// Quota validity: the client renders the live countdown from ExpiresAt; the
	// server flags the expired/locked state (authoritative for enforcement).
	now := time.Now().UTC()
	d.Expired = mgr.IsExpired(u.ExpiresAt, now)
	d.ExpiresSoon = u.ExpiresAt != "" && !d.Expired && mgr.ExpiryRemaining(u.ExpiresAt, now) <= 72*time.Hour
	// Resource usage comes from the persisted sampler snapshot (five-minute
	// CPU average plus latest memory/disk), never from a live Incus sample.
	d.CPUUse, d.MemUse, d.DiskUsed = s.mgr.PanelResources(u.ID)
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
		if s.mgr.IPv6Mode() == cfg.IPv6ModePool {
			// Pool mode: the address comes from the DB (the assignment), not
			// derived — and there is no /112 block to show.
			if u.IPv6Address != "" {
				d.IPv6 = u.IPv6Address
			}
		} else {
			if ipv6, _ := s.mgr.IPv6Addr(u.Name); ipv6 != "" { // pure computation, no incus call
				d.IPv6 = ipv6
			}
			if b, _ := s.mgr.IPv6Block(u.Name); b != nil {
				d.IPv6Block = b.String()
			}
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
	// Snapshots come from Incus (one list call). A failure is non-fatal: the
	// page still renders, and the snapshot modal shows the empty state.
	if snaps, err := s.mgr.SnapshotList(u.Name); err == nil {
		for _, sn := range snaps {
			d.Snapshots = append(d.Snapshots, snapshotRow{Name: sn.Name, CreatedAt: sn.CreatedAt, Size: mgr.HumanBytes(sn.Size)})
		}
	}
	d.SnapshotLimit = s.mgr.SnapshotLimit()
	keys, _ := s.db.ListSSHKeys(u.ID)
	for _, k := range keys {
		d.SSHKeys = append(d.SSHKeys, sshKeyRow{ID: k.ID, Name: k.Name, Key: k.Key, Active: k.Active})
	}
	// The operator's public keys, surfaced read-only in the SSH-key panel. A
	// user must see them (names + contents) to decide which to authorize, but
	// never edits them here. Active on these rows means "this user has already
	// activated it" (a checked grant), not the admin's own store flag.
	if akeys, err := s.db.ListAdminKeys(); err == nil {
		granted := map[int64]bool{}
		if gk, err := s.db.GrantedAdminKeys(u.ID); err == nil {
			for _, k := range gk {
				granted[k.ID] = true
			}
		}
		for _, k := range akeys {
			granted := granted[k.ID]
			d.AdminSSHKeys = append(d.AdminSSHKeys, sshKeyRow{ID: k.ID, Name: k.Name, Key: k.Key, Active: granted})
			if granted {
				d.AdminKeysAnyActive = true
			}
		}
	}
	return d
}
