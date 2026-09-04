package admin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
	"vpsmgr/internal/lx"
	"vpsmgr/internal/mgr"
	"vpsmgr/internal/pw"
)

type ctxKey int

const adminKey ctxKey = 0

// loginDummyHash is compared against when no admin password is set yet so the
// login takes the same time whether or not an admin exists.
var loginDummyHash = func() string { h, _ := pw.Hash("vpsmgr-admin-timing-pad"); return h }()

// pageData is the data handed to the admin templates.
type pageData struct {
	Title       string
	Prefix      string
	Msg         string
	Err         string
	Host        hostView
	Reboot      bool
	Users       []userView
	UserCount   int
	MaxUsers    int
	CapacityPct int
	V4Forward   bool
	Lang        string
	// Colors is the fixed palette of accent colors an admin may assign to a
	// user (see userColorPalette). Empty when never rendered.
	Colors []string
	// IPv6 pool mode: the free addresses offered in the create-user dropdown
	// (empty = not pool mode / pool exhausted). PoolUsed/PoolTotal are the
	// pool fill for display.
	IPv6PoolMode bool
	PoolFree     []string
	PoolUsed     int
	PoolTotal    int
	// AdminKeys is the operator's own SSH-key store (management panel only).
	AdminKeys []sshKeyRow
}

// sshKeyRow is one public key shown in the admin SSH-key management panel.
type sshKeyRow struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Key    string `json:"key"` // clean "type base64" (comment stripped)
	Active bool   `json:"active"`
}

// hostView carries host memory/swap/pool/uptime numbers for the overview cards.
type hostView struct {
	MemTotal  string
	MemUsed   string
	MemPct    string
	SwapTotal string
	SwapUsed  string
	SwapPct   string
	PoolTotal string
	PoolUsed  string
	PoolAvail string
	PoolPct   string
	Uptime    string
}

// userView is one row of the admin user table.
type userView struct {
	Name string
	// Color is the operator-assigned accent color ("" = default). It colors
	// the bold username and the "login panel" button for quick user spotting.
	Color string
	State string
	// Status is the persistent lifecycle state (ready/creating/reinstalling/
	// failed). Non-ready states are shown to the operator so a crashed
	// Add/Reinstall is visible instead of looking like a healthy user.
	Status      string
	Ports       string // full user-port block, e.g. 10700-10799 (tooltip)
	PortsShort  string // compact form, e.g. 107xx
	SSHPort     string
	QuotaCPU    string
	QuotaMem    string
	QuotaDisk   string
	BandwidthGB int // monthly bandwidth quota GiB, 0 = unlimited
	CPUUse      string
	MemUse      string
	DiskUsed    string
	UpGB        string
	DownGB      string
	BWTotal     string // up+down this month, GB — used for table sorting
	IPv6        string
	Procs       int64  // live process count (0 when stopped)
	ProcsLimit  string // per-container pids.max cap, e.g. "4096"
}

func (s *Server) buildPageData(msg, errMsg string) pageData {
	d := pageData{
		Title:     "VPS Manager Admin",
		Prefix:    s.prefix(),
		Msg:       msg,
		Err:       errMsg,
		V4Forward: s.mgr.V4ForwardLive(),
		Colors:    append([]string{}, userColorPalette...),
	}
	hs := s.mgr.HostStats()
	d.Reboot = hs.RebootNeeded
	d.Host = hostView{
		MemTotal:  humanBytes(int64(hs.Mem.MemTotal)),
		MemUsed:   humanBytes(int64(hs.Mem.MemUsed)),
		SwapTotal: humanBytes(int64(hs.Mem.SwapTotal)),
		SwapUsed:  humanBytes(int64(hs.Mem.SwapUsed)),
		PoolTotal: humanBytes(hs.PoolTotal),
		PoolUsed:  humanBytes(hs.PoolUsed),
		PoolAvail: humanBytes(hs.PoolAvail),
		Uptime:    formatUptime(hs.Uptime),
	}
	if hs.Mem.MemTotal > 0 {
		d.Host.MemPct = strconv.Itoa(int(hs.Mem.MemUsed*100/hs.Mem.MemTotal)) + "%"
	}
	if hs.Mem.SwapTotal > 0 {
		d.Host.SwapPct = strconv.Itoa(int(hs.Mem.SwapUsed*100/hs.Mem.SwapTotal)) + "%"
	}
	if hs.PoolTotal > 0 {
		d.Host.PoolPct = strconv.Itoa(int(hs.PoolUsed*100/hs.PoolTotal)) + "%"
	}
	// IPv6 pool mode: offer the free addresses in the create dropdown.
	if s.mgr.IPv6Mode() == cfg.IPv6ModePool {
		d.IPv6PoolMode = true
		d.PoolFree = s.mgr.FreePoolIPv6List()
		if total, used, err := s.mgr.IPv6PoolUsage(); err == nil {
			d.PoolUsed, d.PoolTotal = used, total
		}
	}
	if keys, err := s.mgr.ListAdminKeys(); err == nil {
		for _, k := range keys {
			d.AdminKeys = append(d.AdminKeys, sshKeyRow{ID: k.ID, Name: k.Name, Key: k.Key, Active: k.Active})
		}
	}
	return d
}

func (s *Server) loadUsers(d *pageData) {
	statuses, err := s.mgr.BatchUsers()
	if err != nil {
		d.Err = d.Err + " " + err.Error()
		return
	}
	vs := make([]userView, 0, len(statuses))
	for _, st := range statuses {
		u := st.User
		vs = append(vs, userView{
			Name:        u.Name,
			Color:       u.Color,
			State:       st.State,
			Status:      u.Status,
			Ports:       mgr.UserPorts(u.StartPort, cfg.PortsPerUser),
			PortsShort:  mgr.UserPortsShort(u.StartPort),
			SSHPort:     strconv.Itoa(u.SSHPort),
			QuotaCPU:    mgr.FormatCPU(u.CPU),
			QuotaMem:    strconv.Itoa(u.MemMB) + " MiB",
			QuotaDisk:   strconv.Itoa(u.DiskGB) + " GiB",
			BandwidthGB: u.BandwidthQuotaGB,
			CPUUse:      st.CPUUse,
			MemUse:      st.MemUse,
			DiskUsed:    st.DiskUsed,
			UpGB:        st.UpGB,
			DownGB:      st.DownGB,
			BWTotal:     st.BWTotal,
			IPv6:        st.IPv6,
			Procs:       st.Procs,
			ProcsLimit:  lx.DefaultProcessesLimit,
		})
	}
	d.Users = vs
	d.UserCount = len(vs)
	// Capacity follows the configured user-port block count, not a fixed 200: a
	// narrowed net.user_ports lowers the ceiling, and existing users that fall
	// outside it (from a wider earlier setting) can push the count past 100% —
	// shown honestly as over-capacity.
	d.MaxUsers = s.cfg.UserPortCount()
	if d.MaxUsers > 0 {
		d.CapacityPct = d.UserCount * 100 / d.MaxUsers
		// With only a handful of containers the true percentage (e.g. 1/200)
		// is an invisible sliver; floor it at 2% so the bar is visibly present
		// once there is at least one container. 0 users stays empty.
		if d.UserCount > 0 && d.CapacityPct < 2 {
			d.CapacityPct = 2
		}
	}
}

// formatUptime renders a duration as a static non-ticking string like
// "5d 3h 12m" so the admin panel shows the uptime captured at page load.
func formatUptime(d time.Duration) string {
	if d <= 0 {
		return "-"
	}
	d = d.Round(time.Minute)
	days := d / (24 * time.Hour)
	d -= days * 24 * time.Hour
	hours := d / time.Hour
	d -= hours * time.Hour
	mins := d / time.Minute
	if days > 0 {
		return strconv.FormatInt(int64(days), 10) + "d " + strconv.FormatInt(int64(hours), 10) + "h " + strconv.FormatInt(int64(mins), 10) + "m"
	}
	return strconv.FormatInt(int64(hours), 10) + "h " + strconv.FormatInt(int64(mins), 10) + "m"
}

// humanBytes renders a byte count as a short human string (e.g. "184 MiB").
func humanBytes(b int64) string {
	if b < 0 {
		b = 0
	}
	const unit = 1024
	if b < unit {
		return strconv.FormatInt(b, 10) + " B"
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(b)/float64(div), 'f', 1, 64) + " " + "KMGTPE"[exp:exp+1] + "iB"
}

// storeFlash persists a one-shot banner for the request's admin session.
func (s *Server) storeFlash(r *http.Request, msg, kind string) {
	if c, err := r.Cookie("vpsmgr_admin_session"); err == nil {
		if msg == "" {
			s.flash.Clear(c.Value)
			return
		}
		s.flash.Set(c.Value, msg, kind)
	}
}

// currentAdminHash reads the admin password hash fresh from the DB on every
// login. The CLI (`vps admin-passwd`) and the web UI both write the hash to
// the settings table, so a CLI reset is effective immediately without
// restarting the panel service. Login is low-frequency, so the extra read is
// negligible.
func (s *Server) currentAdminHash() string {
	v, _, err := s.db.GetSetting(db.SettingAdminPassHash)
	if err != nil {
		return ""
	}
	return v
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		ip := clientIP(r)
		s.limiter.prune()
		if !s.limiter.allowed(ip) {
			s.renderStatus(w, r, http.StatusTooManyRequests, "admin_login.html",
				pageData{Title: "Admin Login", Prefix: s.prefix(), Err: s.t(r, "err_too_many")})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		pass := r.FormValue("password")
		// Password-only login: no username. Compare against the configured
		// bcrypt hash; when unset (fresh install before admin-passwd) burn the
		// same bcrypt time as a real compare.
		hash := s.currentAdminHash()
		if hash == "" {
			pw.Verify(loginDummyHash, pass)
			s.render(w, r, "admin_login.html", pageData{Title: "Admin Login", Prefix: s.prefix(), Err: s.t(r, "err_not_configured")})
			return
		}
		if !pw.Verify(hash, pass) {
			s.render(w, r, "admin_login.html", pageData{Title: "Admin Login", Prefix: s.prefix(), Err: s.t(r, "err_bad_login")})
			return
		}
		token, err := s.sessions.create(s.cfg.Panel.SessionDays)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		s.setSessionCookie(w, token)
		_ = s.db.AddAuditLog("000", "session.login")
		s.redirect(w, r, s.p(""), "")
		return
	}
	s.render(w, r, "admin_login.html", pageData{Title: "Admin Login", Prefix: s.prefix()})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("vpsmgr_admin_session"); err == nil {
		s.sessions.delete(c.Value)
	}
	s.clearSessionCookie(w)
	s.redirect(w, r, s.p("/login"), "")
}

// handleOverview renders the admin dashboard. It performs one full batch
// refresh (a handful of incus calls) on every manual page load; there is no
// automatic polling.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	d := s.buildPageData("", "")
	s.loadUsers(&d)
	s.render(w, r, "admin_overview.html", d)
}

func (s *Server) handleFlash(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("vpsmgr_admin_session")
	if err != nil {
		http.Error(w, "", http.StatusUnauthorized)
		return
	}
	msg, kind, data, _ := s.flash.Pop(c.Value)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Msg  string `json:"msg"`
		Kind string `json:"kind"`
		Data string `json:"data"`
	}{msg, kind, data})
}

// handleUserAdd creates a user with the CLI's Add logic and shows the full
// login credentials (panel address, username, password) once in a modal.
// userColorPalette is the fixed set of accent colors an admin can assign to a
// user. Black/white/gray are excluded on purpose (too faint against the
// panel's neutral surfaces); these mid-tone hues stay readable as button
// backgrounds in both light and dark mode. The stored value is the hex string
// itself, and this list is the allowlist — anything else is rejected.
var userColorPalette = []string{
	"#e11d48", // red
	"#ea580c", // orange
	"#d97706", // amber
	"#16a34a", // green
	"#0d9488", // teal
	"#0891b2", // cyan
	"#3b82f6", // blue
	"#7c3aed", // violet
	"#c026d3", // fuchsia
	"#db2777", // pink
}

// validUserColor reports whether color is a member of the fixed palette
// ("" is reserved for "reset to default" and handled by the caller).
func validUserColor(color string) bool {
	for _, c := range userColorPalette {
		if c == color {
			return true
		}
	}
	return false
}

func (s *Server) handleUserAdd(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	name := r.FormValue("name")
	cpu, err := mgr.ParseCPU(r.FormValue("cpu"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	memMB, err := parseMem(r.FormValue("mem"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	diskGB, err := strconv.Atoi(r.FormValue("disk"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+s.t(r, "err_invalid_disk"))
		return
	}
	bandwidthGB, err := mgr.ParseBandwidthGB(r.FormValue("bandwidth"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	// IPv6 dropdown (pool mode only): "auto" (or empty) = first free pool
	// address, "none" = V4-only container, otherwise the picked address.
	ipv6 := strings.TrimSpace(r.FormValue("ipv6"))
	if ipv6 == "auto" {
		ipv6 = ""
	}
	res, err := s.mgr.Add(name, mgr.AddOptions{CPU: cpu, MemMB: memMB, DiskGB: diskGB, BandwidthGB: bandwidthGB, IPv6Addr: ipv6})
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog("000+"+name, "user.create")
	// The password is always auto-generated and shown once. The panel address
	// is taken from the request's own Host (see panelURL), so it matches
	// whatever origin the operator actually used to reach the admin panel.
	cred := "user:      " + res.User.Name +
		"\npassword:  " + res.Password +
		"\npanel:     " + s.panelURL(r, "/"+s.cfg.Panel.URLPath)
	// Carry the username as flash data so the modal can offer "log in as".
	s.redirectModalData(w, r, s.p(""), cred, res.User.Name)
}

func (s *Server) handleUserDel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	name := r.FormValue("name")
	if r.FormValue("confirm") != "1" {
		s.redirect(w, r, s.p(""), "error: please confirm deletion")
		return
	}
	if err := s.mgr.Del(name); err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog("000+"+name, "user.delete")
	s.redirect(w, r, s.p(""), s.t(r, "user_deleted", name))
}

func (s *Server) handleUserQuota(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	name := r.FormValue("name")
	cpu, err := mgr.ParseCPU(r.FormValue("cpu"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	memMB, err := parseMem(r.FormValue("mem"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	diskGB, err := strconv.Atoi(r.FormValue("disk"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+s.t(r, "err_invalid_disk"))
		return
	}
	bandwidthGB, err := mgr.ParseBandwidthGB(r.FormValue("bandwidth"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	if _, err := s.mgr.UpdateQuotasAndBandwidth(name, cpu, memMB, diskGB, bandwidthGB); err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog("000+"+name, "quota.update")
	s.redirect(w, r, s.p(""), s.t(r, "quota_updated", name))
}

// handleUserBandwidthReset zeroes a user's monthly traffic counters without
// touching the resource quotas. If the user was over quota (and thus
// throttled), the reset also lifts the NIC rate limit immediately.
func (s *Server) handleUserBandwidthReset(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	name := r.FormValue("name")
	if err := s.mgr.ResetBandwidth(name); err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog("000+"+name, "bandwidth.reset")
	s.redirect(w, r, s.p(""), s.t(r, "bandwidth_reset", name))
}

// handleUserColor sets or clears a user's accent color. Empty color = reset
// to default; a non-empty color must come from the fixed palette (allowlist,
// never a free-form value). It is triggered from the username in the user
// table — a deliberately low-key entry point, documented rather than surfaced
// as its own button.
func (s *Server) handleUserColor(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	name := r.FormValue("name")
	color := strings.TrimSpace(r.FormValue("color"))
	if color != "" && !validUserColor(color) {
		s.redirect(w, r, s.p(""), "error: "+s.t(r, "err_invalid_color"))
		return
	}
	u, err := s.db.GetUserByName(name)
	if err != nil {
		s.redirect(w, r, s.p(""), "error: user not found")
		return
	}
	if err := s.db.UpdateUserColor(u.ID, color); err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog("000+"+name, "color.update")
	if color == "" {
		s.redirect(w, r, s.p(""), s.t(r, "color_reset", name))
		return
	}
	s.redirect(w, r, s.p(""), s.t(r, "color_updated", name))
}

func (s *Server) handlePower(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	action := r.FormValue("action")
	name := r.FormValue("name")
	if err := s.mgr.Power(name, action); err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog("000+"+name, "power."+action)
	s.redirect(w, r, s.p(""), s.t(r, "power_ok", name, action))
}

// handleResetPanelPass resets a user's panel login password and shows it once.
func (s *Server) handleResetPanelPass(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	name := r.FormValue("name")
	pass, err := s.mgr.ResetPanelPassword(name)
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	panel := s.panelURL(r, "/"+s.cfg.Panel.URLPath)
	_ = s.db.AddAuditLog("000+"+name, "passwd.reset")
	s.redirectModal(w, r, s.p(""), s.t(r, "new_panel_password", name, pass, panel))
}

// handleAdminPass changes the admin panel password (no username). The current
// session is preserved.
func (s *Server) handleAdminPass(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	pass := r.FormValue("new_password")
	confirm := r.FormValue("confirm_password")
	if pass != confirm {
		s.redirect(w, r, s.p(""), "error: "+s.t(r, "err_pass_mismatch"))
		return
	}
	if len(pass) < 14 {
		s.redirect(w, r, s.p(""), "error: "+s.t(r, "err_pass_short"))
		return
	}
	hash, err := pw.Hash(pass)
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	if err := s.db.SetSetting(db.SettingAdminPassHash, hash); err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	// Every other admin session is now stale: keep only the one that changed
	// the password, so a stolen/long-open session cannot outlive the rotation.
	if c, err := r.Cookie("vpsmgr_admin_session"); err == nil {
		s.sessions.clearExcept(c.Value)
	}
	_ = s.db.AddAuditLog("000", "admin.passwd")
	s.redirect(w, r, s.p(""), s.t(r, "admin_pass_changed"))
}

// handleLoginAs ("log in as user" / impersonation) creates a user-panel
// session for the given username and hands the browser its cookie, dropping the
// operator straight into that user's panel — regardless of the user's password.
// The admin's own session cookie is separate and untouched, so returning to the
// admin panel works normally; impersonating another user later simply replaces
// the user-panel cookie. The session is flagged impersonated, so the user
// panel attributes its audit events "000+<user>" and shows a banner.
func (s *Server) handleLoginAs(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	// Impersonation hands the browser the user-panel cookie. Refuse when the
	// user panel is disabled: a bare "/" cookie path would otherwise be sent to
	// every request on the host, including the admin panel. (With no URLPath
	// there is nothing to impersonate into anyway.)
	if s.cfg.Panel.URLPath == "" {
		s.redirect(w, r, s.p(""), "error: user panel is disabled")
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	u, err := s.db.GetUserByName(name)
	if err != nil {
		s.redirect(w, r, s.p(""), "error: user not found")
		return
	}
	sess, err := s.db.CreateImpersonatedSession(u.ID, s.cfg.Panel.SessionDays)
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	// Set the user panel's own session cookie (same name, path and flags as a
	// real login) so the browser becomes that user. Only the user-panel cookie
	// is written here; the admin cookie is left alone.
	userPrefix := "/" + s.cfg.Panel.URLPath
	http.SetCookie(w, &http.Cookie{
		Name:     "vpsmgr_session",
		Value:    sess.Token,
		Path:     userPrefix,
		MaxAge:   s.cfg.Panel.SessionDays * 86400,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
	_ = s.db.AddAuditLog("000+"+u.Name, "session.login")
	http.Redirect(w, r, userPrefix+"/", http.StatusFound)
}

// panelURL renders the user-panel address as the browser sees it: https plus
// the request's own Host (not the configured listen port). This keeps the
// address shown in admin modals correct when the panel is reached through a
// reverse proxy on a custom domain or a different port. The scheme is always
// https — the panel only serves TLS and its session cookie is Secure, so it
// cannot function over plain http regardless of the transport in between.
func (s *Server) panelURL(r *http.Request, prefix string) string {
	return "https://" + r.Host + prefix
}

// parseMem parses a memory string ("512" or "1G") into MiB, mirroring the CLI.
func parseMem(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, strconv.ErrSyntax
	}
	mult := 1
	last := s[len(s)-1]
	switch {
	case last >= '0' && last <= '9':
	case last == 'M' || last == 'm':
		s = s[:len(s)-1]
	case last == 'G' || last == 'g':
		mult = 1024
		s = s[:len(s)-1]
	default:
		return 0, strconv.ErrSyntax
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	}
	n *= mult
	if n < 64 {
		return 0, strconv.ErrSyntax
	}
	return n, nil
}

// ---- domain management ----

// domainView is one row of the admin domain panel.
type domainView struct {
	Domain        string
	Username      string
	UpdatedAt     string // UTC RFC3339; rendered in the browser's timezone
	ProxyProtocol bool
}

type domainsPageData struct {
	Title   string
	Prefix  string
	Msg     string
	Err     string
	Domains []domainView
	Blocked string // blocked-domains list, one domain per line (textarea)
	Lang    string
}

func (s *Server) renderDomains(w http.ResponseWriter, r *http.Request, d domainsPageData) {
	t, err := s.templates()
	if err != nil {
		http.Error(w, "template error: "+err.Error(), 500)
		return
	}
	if d.Lang == "" {
		d.Lang = langEn
		if l, ok := r.Context().Value(langCtxKey).(string); ok && l != "" {
			d.Lang = l
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := t.ExecuteTemplate(w, "admin_domains.html", d); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

// handleDomains renders the admin domain panel: every domain with its owner
// and last-modified time, newest first, plus the blocked-domains list.
func (s *Server) handleDomains(w http.ResponseWriter, r *http.Request) {
	d := domainsPageData{Title: "VPS Manager Admin — Domains", Prefix: s.prefix()}
	all, err := s.mgr.AllDomains()
	if err != nil {
		d.Err = err.Error()
	} else {
		for _, x := range all {
			d.Domains = append(d.Domains, domainView{Domain: x.Domain, Username: x.Username, UpdatedAt: x.UpdatedAt, ProxyProtocol: x.ProxyProtocol})
		}
	}
	if blocked, err := s.db.GetBlockedDomains(); err != nil {
		if d.Err != "" {
			d.Err += " "
		}
		d.Err += err.Error()
	} else {
		d.Blocked = strings.Join(blocked, "\n")
	}
	s.renderDomains(w, r, d)
}

// handleDomainDel deletes a domain (admin path). It finds the owning user and
// removes the domain + its traefik file atomically.
func (s *Server) handleDomainDel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	domain := r.FormValue("domain")
	if err := s.mgr.AdminDelDomain(domain); err != nil {
		s.redirect(w, r, s.p("/domains"), "error: "+err.Error())
		return
	}
	if dmn, err := s.db.GetDomainByDomain(domain); err == nil {
		if owner, err := s.db.GetUserByID(dmn.UserID); err == nil {
			_ = s.db.AddAuditLog("000+"+owner.Name, "domain_update")
		}
	}
	s.redirect(w, r, s.p("/domains"), s.t(r, "domain_deleted", domain))
}

// handleDomainUpdate applies the admin batch PROXY protocol toggle, same
// semantics as the user panel.
func (s *Server) handleDomainUpdate(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	checked := map[string]bool{}
	for _, d := range r.Form["proto"] {
		checked[d] = true
	}
	all, err := s.mgr.AllDomains()
	if err != nil {
		s.redirect(w, r, s.p("/domains"), "error: "+err.Error())
		return
	}
	changed := 0
	for _, x := range all {
		on := checked[x.Domain]
		if on != x.ProxyProtocol {
			if err := s.mgr.AdminSetDomainProtocol(x.Domain, on); err != nil {
				s.redirect(w, r, s.p("/domains"), "error: "+err.Error())
				return
			}
			_ = s.db.AddAuditLog("000+"+x.Username, "domain_update")
			changed++
		}
	}
	if changed == 0 {
		s.redirect(w, r, s.p("/domains"), "ok: no changes")
		return
	}
	s.redirect(w, r, s.p("/domains"), s.t(r, "domains_updated"))
}

// handleBlockedDomains saves the admin blocked-domains list from the textarea.
// Every line is validated individually: invalid lines are skipped (and
// reported by their 1-based line numbers in a flash banner), the valid lines
// are saved. The check lives in mgr.AddDomain, so all add paths are covered.
func (s *Server) handleBlockedDomains(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	valid, bad := mgr.ParseBlockedList(r.FormValue("blocked"))
	if err := s.db.SetBlockedDomains(valid); err != nil {
		s.redirect(w, r, s.p("/domains"), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog("000", "blocked.update")
	if len(bad) > 0 {
		lines := make([]string, 0, len(bad))
		for _, n := range bad {
			lines = append(lines, strconv.Itoa(n))
		}
		s.redirect(w, r, s.p("/domains"), s.t(r, "blocked_skipped_lines", strings.Join(lines, ", ")))
		return
	}
	s.redirect(w, r, s.p("/domains"), s.t(r, "blocked_saved"))
}

// ---- IPv6 pool management ----

type ipv6PoolPageData struct {
	Title  string
	Prefix string
	Msg    string
	Err    string
	Lang   string
	Mode   string // "pool" when pool mode is active, else ""
	Total  int
	Used   int
	Free   int
	Addrs  []mgr.PoolEntries
}

func (s *Server) renderIPv6Pool(w http.ResponseWriter, r *http.Request, d ipv6PoolPageData) {
	t, err := s.templates()
	if err != nil {
		http.Error(w, "template error: "+err.Error(), 500)
		return
	}
	if d.Lang == "" {
		d.Lang = langEn
		if l, ok := r.Context().Value(langCtxKey).(string); ok && l != "" {
			d.Lang = l
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := t.ExecuteTemplate(w, "admin_ipv6pool.html", d); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

// handleIPv6Pool renders the pool management page (list + add box).
func (s *Server) handleIPv6Pool(w http.ResponseWriter, r *http.Request) {
	d := ipv6PoolPageData{Title: "VPS Manager Admin — IPv6 Pool", Prefix: s.prefix()}
	if s.mgr.IPv6Mode() == cfg.IPv6ModePool {
		d.Mode = cfg.IPv6ModePool
	} else if s.mgr.IPv6Mode() == cfg.IPv6ModePrefix {
		d.Mode = cfg.IPv6ModePrefix
	}
	total, used, err := s.mgr.IPv6PoolUsage()
	if err != nil {
		d.Err = err.Error()
	} else {
		d.Total, d.Used = total, used
		d.Free = total - used
	}
	addrs, err := s.mgr.PoolList()
	if err != nil {
		d.Err = d.Err + " " + err.Error()
	} else {
		d.Addrs = addrs
	}
	s.renderIPv6Pool(w, r, d)
}

// handleIPv6PoolAdd batch-adds addresses from the multi-line textarea.
func (s *Server) handleIPv6PoolAdd(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	raw := r.FormValue("addresses")
	var entries []string
	for _, line := range strings.FieldsFunc(raw, func(c rune) bool { return c == '\n' || c == ',' }) {
		line = strings.TrimSpace(line)
		if line != "" {
			entries = append(entries, line)
		}
	}
	added, err := s.mgr.AddPoolIPv6s(entries)
	if err != nil {
		s.redirect(w, r, s.p("/ipv6pool"), "error: "+err.Error())
		return
	}
	if len(added) == 0 {
		s.redirect(w, r, s.p("/ipv6pool"), "ok: no new addresses")
		return
	}
	_ = s.db.AddAuditLog("000", "ipv6pool.add")
	s.redirect(w, r, s.p("/ipv6pool"), s.t(r, "pool_added", len(added)))
}

// handleIPv6PoolDel removes one address from the pool.
func (s *Server) handleIPv6PoolDel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	addr := strings.TrimSpace(r.FormValue("addr"))
	if err := s.mgr.RemovePoolIPv6(addr); err != nil {
		s.redirect(w, r, s.p("/ipv6pool"), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog("000", "ipv6pool.del")
	s.redirect(w, r, s.p("/ipv6pool"), s.t(r, "pool_removed", addr))
}

// ---- audit log ----

type auditPageData struct {
	Title  string
	Prefix string
	Lang   string
}

func (s *Server) renderAudit(w http.ResponseWriter, r *http.Request, d auditPageData) {
	t, err := s.templates()
	if err != nil {
		http.Error(w, "template error: "+err.Error(), 500)
		return
	}
	if d.Lang == "" {
		d.Lang = langEn
		if l, ok := r.Context().Value(langCtxKey).(string); ok && l != "" {
			d.Lang = l
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := t.ExecuteTemplate(w, "admin_audit.html", d); err != nil {
		http.Error(w, err.Error(), 500)
	}
}

// handleAudit renders the audit page shell; rows are fetched chunk-by-chunk by
// the browser from /audit/api so the page never renders thousands of rows.
func (s *Server) handleAudit(w http.ResponseWriter, r *http.Request) {
	s.renderAudit(w, r, auditPageData{Title: "VPS Manager Admin — Audit", Prefix: s.prefix()})
}

// handleAuditAPI returns one chunk of audit rows as JSON for client-side
// rendering (500 per chunk, newest first).
func (s *Server) handleAuditAPI(w http.ResponseWriter, r *http.Request) {
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	if offset < 0 {
		offset = 0
	}
	limit := 500
	if l, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && l > 0 && l <= 1000 {
		limit = l
	}
	rows, err := s.db.ListAuditLog(offset, limit)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	total, _ := s.db.AuditCount()
	type auditRowJSON struct {
		ID        int64  `json:"id"`
		Actor     string `json:"actor"`
		Action    string `json:"action"`
		CreatedAt string `json:"created_at"`
	}
	out := make([]auditRowJSON, 0, len(rows))
	for _, a := range rows {
		out = append(out, auditRowJSON{a.ID, a.Actor, a.Action, a.CreatedAt})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Rows  []auditRowJSON `json:"rows"`
		More  bool           `json:"more"`
		Total int            `json:"total"`
	}{out, offset+len(rows) < total, total})
}

// handleAdminKeys reconciles the operator's SSH-key store from the management
// panel. The body is a JSON array of key rows (ID > 0 updates, ID == 0 adds,
// missing rows are deleted), same contract as the user panel's /ssh-keys.
// Returns the fresh key list so the panel can re-render without a reload.
func (s *Server) handleAdminKeys(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Keys []mgr.SSHKeyInput `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAdminKeys(w, false, "", nil)
		return
	}
	keys, err := s.mgr.SaveAdminKeys(req.Keys)
	if err != nil {
		writeAdminKeys(w, false, err.Error(), nil)
		return
	}
	_ = s.db.AddAuditLog("000", "admin.keys")
	writeAdminKeys(w, true, "", keys)
}

func writeAdminKeys(w http.ResponseWriter, ok bool, errMsg string, keys []db.AdminKey) {
	rows := make([]sshKeyRow, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, sshKeyRow{ID: k.ID, Name: k.Name, Key: k.Key, Active: k.Active})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		OK    bool        `json:"ok"`
		Error string      `json:"error,omitempty"`
		Keys  []sshKeyRow `json:"keys"`
	}{ok, errMsg, rows})
}
