package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"vpsmgr/internal/db"
	"vpsmgr/internal/mgr"
	"vpsmgr/internal/pw"
)

func itoa(n int) string { return strconv.Itoa(n) }

type ctxKey int

const userKey ctxKey = 0

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
		u, err := s.db.SessionUser(c.Value)
		if err != nil {
			s.clearSessionCookie(w)
			s.redirect(w, r, s.p("/login"), "")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	}
}

// requirePost rejects everything but POST so no state-changing action can be
// triggered by a top-level GET navigation (CSRF via SameSite=Lax). A wrong
// method gets the same bare 404 as any other wrong path — never a 405, which
// would advertise that a POST-only endpoint exists here.
func (s *Server) requirePost(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			featureless404(w)
			return
		}
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
	s.render(w, r, "overview.html", s.buildData(u, "", ""))
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
		_ = s.db.AddAuditLog(u.Name, "power."+action)
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
	_ = s.db.AddAuditLog(u.Name, "reinstall")
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
	_ = s.db.AddAuditLog(u.Name, "reset_root_password")
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
	_ = s.db.AddAuditLog(u.Name, "domain_update")
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
	_ = s.db.AddAuditLog(u.Name, "domain_update")
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
	_ = s.db.AddAuditLog(u.Name, "domain_update")
	s.redirect(w, r, s.p(""), "ok: domain removed")
}

// handleInitScript saves the current user's custom init script (run inside
// their container after a reinstall). It always operates on the session's own
// user — the form carries no name, so no one can edit another user's script.
func (s *Server) handleInitScript(w http.ResponseWriter, r *http.Request) {
	u := s.currentUser(r)
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if err := s.mgr.SetInitScript(u.Name, r.FormValue("script")); err != nil {
		s.redirect(w, r, s.p(""), "error: "+err.Error())
		return
	}
	s.redirect(w, r, s.p(""), "ok: init script saved")
}

// handleImages returns the OS images available for reinstall. It is fetched
// lazily when the user opens the reinstall dialog, not on every page load, so
// an Incus image listing only happens when actually needed.
func (s *Server) handleImages(w http.ResponseWriter, r *http.Request) {
	imgs := s.mgr.Images()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(struct {
		Images  []mgr.ManagedImage `json:"images"`
		Default string             `json:"default"`
	}{imgs, s.cfg.Incus.Image})
}
