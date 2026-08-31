package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
	"vpsmgr/internal/mgr"
	"vpsmgr/internal/pw"
)

func itoa(n int) string { return strconv.Itoa(n) }

type ctxKey int

const (
	userKey ctxKey = 0
	// impersonatedKey marks a session created by the operator ("log in as
	// user"). When set, audit events are attributed "000+<user>" and the panel
	// shows a "logged in as" banner.
	impersonatedKey ctxKey = 1
)

// loginDummyHash is compared against when the username is unknown so that a
// login attempt takes the same time whether or not the account exists,
// defeating username enumeration via response timing.
var loginDummyHash = func() string { h, _ := pw.Hash("vpsmgr-timing-pad"); return h }()

// storeFlash persists a one-shot banner for the request's session. An empty
// msg clears any pending banner (e.g. on logout). Banners live in a short-lived
// in-memory store and are fetched by the frontend, so they never leak into
// URLs, cookies or browser history.
func (s *Server) storeFlash(r *http.Request, msg, kind string) {
	if c, err := r.Cookie("vpsmgr_session"); err == nil {
		if msg == "" {
			s.flash.Clear(c.Value)
			return
		}
		s.flash.Set(c.Value, msg, kind)
	}
}

func (s *Server) handleFlash(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie("vpsmgr_session")
	if err != nil {
		http.Error(w, "", http.StatusUnauthorized)
		return
	}
	msg, kind, _ := s.flash.Pop(c.Value)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Msg  string `json:"msg"`
		Kind string `json:"kind"`
	}{msg, kind})
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("vpsmgr_session")
		if err != nil {
			s.redirect(w, r, s.p("/login"), "")
			return
		}
		u, imp, err := s.db.SessionWithFlag(c.Value)
		if err != nil {
			s.clearSessionCookie(w)
			s.redirect(w, r, s.p("/login"), "")
			return
		}
		ctx := context.WithValue(r.Context(), userKey, u)
		ctx = context.WithValue(ctx, impersonatedKey, imp)
		next(w, r.WithContext(ctx))
	}
}

// isImpersonated reports whether the current session was created by the
// operator ("log in as user").
func (s *Server) isImpersonated(r *http.Request) bool {
	v, _ := r.Context().Value(impersonatedKey).(bool)
	return v
}

// auditActor returns the actor name for an audit event: the plain username for
// a user's own action, or "000+<username>" when the operator is acting as that
// user (impersonation). Mirrors the admin panel's "000+" convention.
func (s *Server) auditActor(r *http.Request, name string) string {
	if s.isImpersonated(r) {
		return "000+" + name
	}
	return name
}

// requirePost rejects everything but POST so no state-changing action can be
// triggered by a top-level GET navigation (CSRF via SameSite=Lax). A wrong
// method gets the same bare 404 as any other wrong path — never a 405, which
// would advertise that a POST-only endpoint exists here.
//
// It also caps the request body: every panel form is small (the largest,
// init script, is bounded to 64 KiB at the manager layer), so a 1 MiB limit
// rejects oversized submissions early — before ParseForm allocates anything.
func (s *Server) requirePost(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			featureless404(w)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		next(w, r)
	}
}

func (s *Server) currentUser(r *http.Request) *db.User {
	u, _ := r.Context().Value(userKey).(*db.User)
	return u
}

func (s *Server) setSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "vpsmgr_session",
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
		Name:     "vpsmgr_session",
		Value:    "",
		MaxAge:   -1,
		Path:     s.prefix(),
		HttpOnly: true,
		Secure:   true,
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		ip := clientIP(r)
		s.limiter.prune()
		if !s.limiter.allowed(ip) {
			s.renderStatus(w, r, http.StatusTooManyRequests, "login.html",
				pageData{Title: "Login", Prefix: s.prefix(), Err: s.t(r, "err_too_many")})
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		name := r.FormValue("username")
		pass := r.FormValue("password")
		u, err := s.db.GetUserByName(name)
		var ok bool
		if err == nil {
			ok = pw.Verify(u.PassHash, pass)
		} else {
			// Burn the same bcrypt time as a real check so unknown usernames
			// are not distinguishable by timing.
			pw.Verify(loginDummyHash, pass)
		}
		if ok {
			sess, err := s.db.CreateSession(u.ID, s.cfg.Panel.SessionDays)
			if err == nil {
				s.setSessionCookie(w, sess.Token)
				_ = s.db.AddAuditLog(u.Name, "session.login")
				s.redirect(w, r, s.p(""), "")
				return
			}
		}
		s.render(w, r, "login.html", pageData{Title: "Login", Prefix: s.prefix(), Err: s.t(r, "err_bad_login")})
		return
	}
	s.render(w, r, "login.html", pageData{Title: "Login", Prefix: s.prefix()})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("vpsmgr_session"); err == nil {
		s.db.DeleteSession(c.Value)
	}
	s.clearSessionCookie(w)
	s.redirect(w, r, s.p("/login"), "")
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	d := s.buildData(u, "", "")
	d.Impersonated = s.isImpersonated(r)
	d.AdminPrefix = "/" + s.cfg.Panel.AdminPath
	s.render(w, r, "overview.html", d)
}

func (s *Server) handlePower(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	action := r.FormValue("action")
	var msg string
	if err := s.mgr.Power(u.Name, action); err != nil {
		msg = "error: " + err.Error()
	} else {
		_ = s.db.AddAuditLog(s.auditActor(r, u.Name), "power."+action)
		msg = "ok: " + action
	}
	s.redirect(w, r, s.p(""), msg)
}

func (s *Server) handleReinstall(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if r.FormValue("confirm") != "1" {
		s.redirect(w, r, s.p(""), "error: please confirm reinstall")
		return
	}
	pass, err := s.mgr.Reinstall(u.Name, r.FormValue("image"))
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog(s.auditActor(r, u.Name), "reinstall")
	s.redirectModal(w, r, s.p(""), s.t(r, "reinstall_done", pass))
}

// handlePanelPassword changes only the panel login password (must be >= 14
// chars) and kicks all other sessions. Container root password is untouched.
func (s *Server) handlePanelPassword(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
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
		s.redirect(w, r, s.p(""), "error: panel password must be at least 14 characters")
		return
	}
	token := ""
	if c, err := r.Cookie("vpsmgr_session"); err == nil {
		token = c.Value
	}
	if err := s.mgr.ChangePanelPassword(u.Name, pass, token); err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog(s.auditActor(r, u.Name), "passwd.change")
	s.redirect(w, r, s.p(""), "ok: panel password changed")
}

// handleRootReset regenerates the container root password and shows it once
// in a modal.
func (s *Server) handleRootReset(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	pass, err := s.mgr.ResetRootPassword(u.Name)
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog(s.auditActor(r, u.Name), "reset_root_password")
	s.redirectModal(w, r, s.p(""), s.t(r, "new_root_password", pass))
}

func (s *Server) handleDomainAdd(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	proxyProtocol := r.FormValue("proxy_protocol") == "1"
	if err := s.mgr.AddDomain(u.Name, r.FormValue("domain"), proxyProtocol); err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog(s.auditActor(r, u.Name), "domain_update")
	s.redirect(w, r, s.p(""), "ok: domain added")
}

// handleDomainUpdate applies the batch PROXY protocol toggle: the form posts
// one `proto` checkbox per domain (only the checked ones), the handler diffs
// them against the DB and applies the changed ones, each atomically.
func (s *Server) handleDomainUpdate(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	checked := map[string]bool{}
	for _, d := range r.Form["proto"] {
		checked[d] = true
	}
	domains, err := s.db.ListDomains(u.ID)
	if err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	changed := 0
	for _, dmn := range domains {
		on := checked[dmn.Domain]
		if on != dmn.ProxyProtocol {
			if err := s.mgr.SetDomainProtocol(u.Name, dmn.Domain, on); err != nil {
				s.redirect(w, r, s.p(""), "error: "+err.Error())
				return
			}
			changed++
		}
	}
	if changed == 0 {
		s.redirect(w, r, s.p(""), "ok: no changes")
		return
	}
	_ = s.db.AddAuditLog(s.auditActor(r, u.Name), "domain_update")
	s.redirect(w, r, s.p(""), "ok: domain settings saved")
}

func (s *Server) handleDomainDel(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := s.mgr.DelDomain(u.Name, r.FormValue("domain")); err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	_ = s.db.AddAuditLog(s.auditActor(r, u.Name), "domain_update")
	s.redirect(w, r, s.p(""), "ok: domain removed")
}

// handleInitScript saves the current user's custom init script (run inside
// their container after a reinstall). It always operates on the session's own
// user — the form carries no name, so no one can edit another user's script.
// The form POST (page reload) is kept for compatibility; the machine panel
// uses a JSON POST that answers with {ok,error} so the modal can stay open.
func (s *Server) handleInitScript(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	jsonReq := strings.HasPrefix(r.Header.Get("Content-Type"), "application/json")
	var script string
	if jsonReq {
		var req struct {
			Script string `json:"script"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeInitScriptJSON(w, false, "bad request")
			return
		}
		script = req.Script
	} else {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		script = r.FormValue("script")
	}
	if err := s.mgr.SetInitScript(u.Name, script); err != nil {
		if jsonReq {
			writeInitScriptJSON(w, false, err.Error())
		} else {
			s.redirect(w, r, s.p(""), "error: "+err.Error())
		}
		return
	}
	_ = s.db.AddAuditLog(s.auditActor(r, u.Name), "init_script")
	if jsonReq {
		writeInitScriptJSON(w, true, "")
	} else {
		s.redirect(w, r, s.p(""), "ok: init script saved")
	}
}

func writeInitScriptJSON(w http.ResponseWriter, ok bool, errMsg string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}{ok, errMsg})
}

// handleImages returns the OS images available for reinstall. It is fetched
// lazily when the user opens the reinstall dialog, not on every page load, so
// an Incus image listing only happens when actually needed. Default is the
// configured image when it exists, else the first offered one, so the picker
// always has a checked entry.
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	imgs := s.mgr.Images()
	def := s.cfg.Incus.Image
	if len(imgs) > 0 && !containsAlias(imgs, def) {
		def = imgs[0].Alias
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Images  []mgr.ManagedImage `json:"images"`
		Default string             `json:"default"`
	}{imgs, def})
}

func containsAlias(imgs []mgr.ManagedImage, alias string) bool {
	for _, im := range imgs {
		if im.Alias == alias {
			return true
		}
	}
	return false
}

// handleStats returns the last 24h of per-minute resource history for the
// current user, for the usage charts on the overview page. Pure DB read — no
// Incus call. Points are [{m, c, y, r, t}] with minute-floored unix seconds,
// CPU percent (-1 = unknown), memory MiB (-1 = unknown) and per-minute rx/tx
// bytes derived from the cumulative counter deltas.
func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	points, err := s.mgr.ChartHistory(u.ID, time.Now().Add(-24*time.Hour))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Points []mgr.ChartPoint `json:"points"`
	}{points})
}

// handleSnapshot creates a disk-only snapshot of the user's container. The
// snapshot name is auto-generated, so the form carries no user input.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	var msg string
	if err := s.mgr.SnapshotCreate(u.Name); err != nil {
		msg = "error: " + err.Error()
	} else {
		_ = s.db.AddAuditLog(s.auditActor(r, u.Name), "snapshot.create")
		msg = "ok: checkpoint created"
	}
	s.redirect(w, r, s.p(""), msg)
}

// handleSnapshotDel deletes a snapshot by name.
func (s *Server) handleSnapshotDel(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	name := r.FormValue("name")
	var msg string
	if err := s.mgr.SnapshotDelete(u.Name, name); err != nil {
		msg = "error: " + err.Error()
	} else {
		_ = s.db.AddAuditLog(s.auditActor(r, u.Name), "snapshot.delete")
		msg = "ok: checkpoint deleted"
	}
	s.redirect(w, r, s.p(""), msg)
}

// handleSnapshotRestore restores the container disk from a snapshot. The
// container is stopped first if running, then started again afterwards.
func (s *Server) handleSnapshotRestore(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	name := r.FormValue("name")
	var msg string
	if err := s.mgr.SnapshotRestore(u.Name, name); err != nil {
		msg = "error: " + err.Error()
	} else {
		_ = s.db.AddAuditLog(s.auditActor(r, u.Name), "snapshot.restore")
		msg = "ok: checkpoint restored"
	}
	s.redirect(w, r, s.p(""), msg)
}

// handleSSHKeys reconciles the user's SSH-key set from the management panel
// AND which of the operator's admin keys the user has activated. The body is a
// JSON object: `keys` is the array of user key rows (ID > 0 updates, ID == 0
// adds, missing rows are deleted), `adminKeys` the list of checked admin-key
// IDs. Returns the fresh user-key list plus the granted admin-key IDs so the
// panel can re-render without a page reload.
func (s *Server) handleSSHKeys(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	var req struct {
		Keys      []mgr.SSHKeyInput `json:"keys"`
		AdminKeys []int64           `json:"adminKeys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeSSHKeys(w, false, "", "", nil, nil)
		return
	}
	keys, err := s.mgr.SaveSSHKeys(u.Name, req.Keys)
	if err != nil {
		writeSSHKeys(w, false, err.Error(), "", nil, nil)
		return
	}
	granted, err := s.mgr.SaveAdminKeyGrants(u.Name, req.AdminKeys)
	if err != nil {
		writeSSHKeys(w, false, err.Error(), "", nil, nil)
		return
	}
	_ = s.db.AddAuditLog(s.auditActor(r, u.Name), "ssh_keys")
	// Apply the selection immediately. Both the user's own keys and the
	// operator's keys are fully synced by marker (activated ones written with
	// their marker, stale/deactivated ones removed), so this runs even when
	// nothing is active — deactivating a key must purge it from the machine.
	// Keys the user added by hand (no marker) are never touched. Best-effort:
	// keys persist in the DB either way, so a save must not fail just because
	// the container is stopped or unreachable.
	warn := ""
	var userActive []string
	for _, k := range keys {
		if k.Active {
			userActive = append(userActive, k.Key)
		}
	}
	if err := s.mgr.ApplySSHKeys(u.Name, userActive, granted); err != nil {
		warn = s.t(r, "ssh_keys_apply_warn")
	}
	ids := make([]int64, 0, len(granted))
	for _, k := range granted {
		ids = append(ids, k.ID)
	}
	writeSSHKeys(w, true, "", warn, keys, ids)
}

func writeSSHKeys(w http.ResponseWriter, ok bool, errMsg, warn string, keys []db.SSHKey, adminIDs []int64) {
	rows := make([]sshKeyRow, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, sshKeyRow{ID: k.ID, Name: k.Name, Key: k.Key, Active: k.Active})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		OK        bool        `json:"ok"`
		Error     string      `json:"error,omitempty"`
		Warning   string      `json:"warning,omitempty"`
		Keys      []sshKeyRow `json:"keys"`
		AdminKeys []int64     `json:"adminKeys"`
	}{ok, errMsg, warn, rows, adminIDs})
}

// handleNotes serves the user's sticky-notes blob. GET returns the stored
// encrypted envelope (empty = never enabled or reset); POST stores a new
// envelope. The payload is opaque to the server — encryption happens in the
// browser — so the only validation is the size cap.
func (s *Server) handleNotes(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if r.Method == http.MethodGet {
		data, _ := s.db.GetStickyNotes(u.ID)
		writeNotesJSON(w, true, "", data != "", data)
		return
	}
	var req struct {
		Data string `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeNotesJSON(w, false, "bad request", false, "")
		return
	}
	if len(req.Data) > cfg.MaxNotesBlobBytes {
		writeNotesJSON(w, false, "notes_full", true, "")
		return
	}
	if err := s.db.SetStickyNotes(u.ID, req.Data); err != nil {
		writeNotesJSON(w, false, "storage error", true, "")
		return
	}
	writeNotesJSON(w, true, "", true, req.Data)
}

// handleNotesReset clears the sticky-notes blob, returning the account to the
// never-enabled state (used by the "forgot password" flow after the user
// confirms — the encrypted content becomes unrecoverable).
func (s *Server) handleNotesReset(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if err := s.db.SetStickyNotes(u.ID, ""); err != nil {
		writeNotesJSON(w, false, "storage error", true, "")
		return
	}
	_ = s.db.AddAuditLog(s.auditActor(r, u.Name), "notes_reset")
	writeNotesJSON(w, true, "", false, "")
}

func writeNotesJSON(w http.ResponseWriter, ok bool, errMsg string, has bool, data string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
		Has   bool   `json:"has"`
		Data  string `json:"data"`
	}{ok, errMsg, has, data})
}
