package admin

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
	"vpsmgr/internal/mgr"
	"vpsmgr/internal/pw"
)

const testAdminSecret = "Adm1n-SecretX"

// testHost is the Host header httptest.NewRequest assigns by default, used to
// build a matching Origin for the same-origin CSRF case.
const testHost = "example.com"

// newTestServer builds an admin Server against a temp DB and points
// VPSMGR_CONFIG at a temp config file so the per-login DB read in
// currentAdminHash() stays isolated from the host's real config/db.
func newTestServer(t *testing.T) (*Server, *db.DB) {
	t.Helper()
	cfgPath := t.TempDir() + "/config.yaml"
	t.Setenv("VPSMGR_CONFIG", cfgPath)
	t.Setenv("VPSMGR_TRAEFIK_DIR", t.TempDir())
	c := cfg.Default()
	c.Panel.URLPath = "UserSecRet99"
	c.Panel.AdminPath = testAdminSecret
	c.Panel.PublicIP = "127.0.0.1"
	c.Panel.SessionDays = 3
	if err := cfg.Save(c); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return New(c, d, mgr.New(c, d)), d
}

// setAdminPass stores the admin password hash in the DB settings table,
// mirroring what `vps admin-passwd` / the web UI write.
func setAdminPass(t *testing.T, srv *Server, pass string) {
	t.Helper()
	if err := srv.db.SetSetting(db.SettingAdminPassHash, mustHash(t, pass)); err != nil {
		t.Fatal(err)
	}
}

func doReq(t *testing.T, h http.Handler, method, target string, form url.Values, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func adminLogin(t *testing.T, h http.Handler, prefix, pass string) *http.Cookie {
	t.Helper()
	rr := doReq(t, h, http.MethodPost, prefix+"/login", url.Values{"password": {pass}}, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("admin login = %d, want 302 (body %s)", rr.Code, rr.Body.String())
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == "vpsmgr_admin_session" {
			return c
		}
	}
	t.Fatal("no admin session cookie")
	return nil
}

func TestAdminLoginAndSession(t *testing.T) {
	srv, _ := newTestServer(t)
	setAdminPass(t, srv, "correct-horse-battery")
	h := srv.Handler()
	prefix := "/" + testAdminSecret

	// Login page renders.
	rr := doReq(t, h, http.MethodGet, prefix+"/login", nil, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Admin") {
		t.Fatalf("GET %s/login = %d, want login page", prefix, rr.Code)
	}
	// Wrong password: no redirect, error shown (English by default).
	rr = doReq(t, h, http.MethodPost, prefix+"/login", url.Values{"password": {"nope"}}, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "invalid admin password") {
		t.Fatalf("bad admin login: code=%d body=%s", rr.Code, rr.Body.String())
	}
	// Correct password: 302 + session cookie scoped to admin prefix.
	rr = doReq(t, h, http.MethodPost, prefix+"/login", url.Values{"password": {"correct-horse-battery"}}, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("admin login = %d, want 302", rr.Code)
	}
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "vpsmgr_admin_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no admin session cookie")
	}
	if cookie.Path != prefix {
		t.Fatalf("cookie Path = %q, want %q", cookie.Path, prefix)
	}
	// Overview with the cookie renders the admin dashboard (no users yet).
	rr = doReq(t, h, http.MethodGet, prefix, nil, cookie)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Admin") {
		t.Fatalf("GET %s with session = %d, want overview", prefix, rr.Code)
	}
}

func TestAdminRequiresSession(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testAdminSecret
	// Without a session the admin root redirects to the admin login.
	rr := doReq(t, h, http.MethodGet, prefix, nil, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("GET %s = %d, want redirect", prefix, rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != prefix+"/login" {
		t.Fatalf("Location = %q, want %q", loc, prefix+"/login")
	}
}

func TestAdminPasswordChange(t *testing.T) {
	srv, _ := newTestServer(t)
	setAdminPass(t, srv, "old-pass-12345678")
	h := srv.Handler()
	prefix := "/" + testAdminSecret
	sess := adminLogin(t, h, prefix, "old-pass-12345678")

	// Mismatched confirmation -> redirect with flash, no change.
	rr := doReq(t, h, http.MethodPost, prefix+"/admin-pass",
		url.Values{"new_password": {"new-pass-123456789"}, "confirm_password": {"different-12345"}}, sess)
	if rr.Code != http.StatusFound {
		t.Fatalf("admin-pass mismatch = %d, want 302", rr.Code)
	}
	if !pw.Verify(storedHash(t, srv), "old-pass-12345678") {
		t.Fatal("password changed despite mismatch")
	}
	// Successful change persists the new hash in the DB settings table.
	rr = doReq(t, h, http.MethodPost, prefix+"/admin-pass",
		url.Values{"new_password": {"new-pass-123456789"}, "confirm_password": {"new-pass-123456789"}}, sess)
	if rr.Code != http.StatusFound {
		t.Fatalf("admin-pass = %d, want 302", rr.Code)
	}
	if !pw.Verify(storedHash(t, srv), "new-pass-123456789") {
		t.Fatal("new password hash not stored")
	}
}

// storedHash reads the admin password hash back from the DB settings table.
func storedHash(t *testing.T, srv *Server) string {
	t.Helper()
	v, ok, err := srv.db.GetSetting(db.SettingAdminPassHash)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("no admin password hash stored")
	}
	return v
}

func TestAdminLogout(t *testing.T) {
	srv, _ := newTestServer(t)
	setAdminPass(t, srv, "logout-pass-12345")
	h := srv.Handler()
	prefix := "/" + testAdminSecret
	sess := adminLogin(t, h, prefix, "logout-pass-12345")

	rr := doReq(t, h, http.MethodPost, prefix+"/logout", nil, sess)
	if rr.Code != http.StatusFound {
		t.Fatalf("logout = %d, want 302", rr.Code)
	}
	// Session is gone: root now redirects to login again.
	rr = doReq(t, h, http.MethodGet, prefix, nil, sess)
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != prefix+"/login" {
		t.Fatalf("after logout, GET %s = %d (loc %q), want redirect to login", prefix, rr.Code, rr.Header().Get("Location"))
	}
}

func TestSessionStoreExpiry(t *testing.T) {
	s := newSessionStore(10)
	tok, err := s.create(0) // 0 days -> expires immediately
	if err != nil {
		t.Fatal(err)
	}
	if s.valid(tok) {
		t.Fatal("zero-length session should be expired")
	}
	tok2, err := s.create(3)
	if err != nil {
		t.Fatal(err)
	}
	if !s.valid(tok2) {
		t.Fatal("3-day session should be valid")
	}
	s.delete(tok2)
	if s.valid(tok2) {
		t.Fatal("deleted session should be invalid")
	}
}

func TestAdminFeatureless404OutsidePrefix(t *testing.T) {
	// The admin handler itself must never serve content off its prefix. The
	// full dispatcher (user + admin + 404) lives in main; here we assert the
	// admin handler returns a bare 404 for non-prefixed paths.
	srv, _ := newTestServer(t)
	h := srv.Handler()
	for _, p := range []string{"/", "/login", "/admin", "/" + testAdminSecret + "x"} {
		rr := doReq(t, h, http.MethodGet, p, nil, nil)
		if rr.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want bare 404", p, rr.Code)
		}
		if body := rr.Body.String(); body != "" {
			t.Fatalf("GET %s body = %q, want empty", p, body)
		}
	}
}

func mustHash(t *testing.T, pass string) string {
	t.Helper()
	h, err := pw.Hash(pass)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// TestLanguageSwitch verifies the admin language is resolved from ?lang=, the
// cookie and the browser header (mirroring the user panel), and that an
// explicit ?lang= choice persists in a scoped cookie.
func TestFormatUptime(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "-"},
		{-1 * time.Second, "-"},
		{5 * time.Minute, "0h 5m"},
		{90 * time.Minute, "1h 30m"},
		{48*time.Hour + 73*time.Minute, "2d 1h 13m"},
		{10*24*time.Hour + 3*time.Hour + 29*time.Minute, "10d 3h 29m"},
	}
	for _, c := range cases {
		if got := formatUptime(c.d); got != c.want {
			t.Errorf("formatUptime(%v) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestLanguageSwitch(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testAdminSecret

	// zh browser -> zh login page.
	req := httptest.NewRequest(http.MethodGet, prefix+"/login", nil)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "管理员登录") {
		t.Fatalf("zh login page missing Chinese title")
	}

	// English browser (or no header) -> en page.
	rr = doReq(t, h, http.MethodGet, prefix+"/login", nil, nil)
	if !strings.Contains(rr.Body.String(), "Admin Login") {
		t.Fatalf("default login page missing English title")
	}

	// Explicit ?lang=en on a zh browser wins and sets the cookie.
	req = httptest.NewRequest(http.MethodGet, prefix+"/login?lang=en", nil)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "Admin Login") {
		t.Fatalf("?lang=en did not switch the page to English")
	}
	var langCookieFound *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == langCookie {
			v := *c
			langCookieFound = &v
		}
	}
	if langCookieFound == nil || langCookieFound.Value != langEn {
		t.Fatalf("?lang=en did not persist the %s cookie", langCookie)
	}

	// The cookie overrides the zh browser header.
	req = httptest.NewRequest(http.MethodGet, prefix+"/login", nil)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.AddCookie(langCookieFound)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "Admin Login") {
		t.Fatalf("cookie did not override browser language")
	}
}

// TestWrongMethodOnPostOnlyRouteIsBare404 verifies that a non-POST request to a
// POST-only admin route (with a valid session) answers with the same bare 404
// as any other wrong path — never a 405, which would advertise the POST-only
// endpoint.
func TestWrongMethodOnPostOnlyRouteIsBare404(t *testing.T) {
	srv, _ := newTestServer(t)
	setAdminPass(t, srv, "correct-horse-battery")
	h := srv.Handler()
	prefix := "/" + testAdminSecret
	sess := adminLogin(t, h, prefix, "correct-horse-battery")

	rr := doReq(t, h, http.MethodGet, prefix+"/user-add", nil, sess)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /user-add with session = %d, want 404", rr.Code)
	}
	if body := rr.Body.String(); body != "" {
		t.Fatalf("GET /user-add body = %q, want empty", body)
	}
	if hdr := rr.Header().Get("Allow"); hdr != "" {
		t.Fatalf("GET /user-add sets Allow header = %q, want none", hdr)
	}
}

func TestDomainsPageAndToggle(t *testing.T) {
	srv, d := newTestServer(t)
	setAdminPass(t, srv, "correct-horse-battery")
	h := srv.Handler()
	prefix := "/" + testAdminSecret

	u, err := d.CreateUser("alice", "h", "10.42.0.2", 1, 30001, 10000, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddDomain(u.ID, "example.com", false); err != nil {
		t.Fatal(err)
	}
	cookie := adminLogin(t, h, prefix, "correct-horse-battery")

	// The domains page lists the domain + owner.
	rr := doReq(t, h, http.MethodGet, prefix+"/domains", nil, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /domains = %d", rr.Code)
	}
	for _, want := range []string{"example.com", "alice"} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("domains page missing %q", want)
		}
	}

	// Admin batch toggle enables proxy protocol.
	rr = doReq(t, h, http.MethodPost, prefix+"/domain-update",
		url.Values{"proto": {"example.com"}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("domain-update = %d, want 302", rr.Code)
	}
	dmn, err := d.GetDomainByDomain("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !dmn.ProxyProtocol {
		t.Error("admin toggle did not enable proxy protocol")
	}

	// Admin delete removes the domain.
	rr = doReq(t, h, http.MethodPost, prefix+"/domain-del",
		url.Values{"domain": {"example.com"}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("domain-del = %d, want 302", rr.Code)
	}
	if _, err := d.GetDomainByDomain("example.com"); err == nil {
		t.Error("domain still present after admin delete")
	}
}

func TestAuditPageAndAPI(t *testing.T) {
	srv, d := newTestServer(t)
	setAdminPass(t, srv, "correct-horse-battery")
	h := srv.Handler()
	prefix := "/" + testAdminSecret
	if err := d.AddAuditLog("alice", "reinstall"); err != nil {
		t.Fatal(err)
	}
	if err := d.AddAuditLog("000+alice", "power.stop"); err != nil {
		t.Fatal(err)
	}
	cookie := adminLogin(t, h, prefix, "correct-horse-battery")

	// The page is a shell — no rows server-rendered.
	rr := doReq(t, h, http.MethodGet, prefix+"/audit", nil, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /audit = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "auditBody") {
		t.Error("audit page missing the client-rendered table body")
	}
	if strings.Contains(rr.Body.String(), "（加载中…）") {
		t.Error("audit page title should not carry a static loading marker")
	}

	// The API returns the rows newest first, with total/more, and no target.
	rr = doReq(t, h, http.MethodGet, prefix+"/audit/api?offset=0&limit=10", nil, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /audit/api = %d", rr.Code)
	}
	b := rr.Body.String()
	for _, want := range []string{`"actor":"000+alice"`, `"action":"power.stop"`, `"total":3`, `"more":false`, `"actor":"000"`, `"action":"session.login"`} {
		if !strings.Contains(b, want) {
			t.Errorf("audit api missing %s:\n%s", want, b)
		}
	}
	if strings.Contains(b, `"target"`) {
		t.Errorf("audit api should not include a target field:\n%s", b)
	}
}

// doReqOrigin is doReq with an Origin header (simulating a browser cross-origin
// POST).
func doReqOrigin(t *testing.T, h http.Handler, method, target string, form url.Values, cookie *http.Cookie, origin string) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(method, target, body)
	req.Header.Set("Origin", origin)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

func TestAdminCSRFRejectsCrossOriginPOST(t *testing.T) {
	srv, _ := newTestServer(t)
	setAdminPass(t, srv, "correct-horse-battery")
	h := srv.Handler()
	prefix := "/" + testAdminSecret
	cookie := adminLogin(t, h, prefix, "correct-horse-battery")

	// A POST with a mismatched Origin is rejected before any handler runs.
	rr := doReqOrigin(t, h, http.MethodPost, prefix+"/user-quota",
		url.Values{"name": {"alice"}, "cpu": {"2"}, "mem": {"2048"}, "disk": {"20"}, "bandwidth": {"0"}},
		cookie, "https://evil.example.com")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST = %d, want 403", rr.Code)
	}

	// Same thing via Sec-Fetch-Site.
	rr = doReqOrigin(t, h, http.MethodPost, prefix+"/user-quota",
		url.Values{"name": {"alice"}, "cpu": {"2"}, "mem": {"2048"}, "disk": {"20"}, "bandwidth": {"0"}},
		cookie, "https://evil.example.com")
	req := httptest.NewRequest(http.MethodPost, prefix+"/user-quota",
		strings.NewReader(url.Values{"name": {"alice"}, "cpu": {"2"}, "mem": {"2048"}, "disk": {"20"}, "bandwidth": {"0"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-site POST = %d, want 403", rr.Code)
	}

	// A same-origin POST (matching Origin) is allowed through to the handler.
	rr = doReqOrigin(t, h, http.MethodPost, prefix+"/user-quota",
		url.Values{"name": {"alice"}, "cpu": {"2"}, "mem": {"2048"}, "disk": {"20"}, "bandwidth": {"0"}},
		cookie, "https://"+testHost)
	if rr.Code == http.StatusForbidden {
		t.Fatal("same-origin POST must not be blocked by CSRF")
	}

	// Login CSRF is covered too: a cross-origin login POST is rejected.
	rr = doReqOrigin(t, h, http.MethodPost, prefix+"/login",
		url.Values{"password": {"correct-horse-battery"}}, nil, "https://evil.example.com")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login POST = %d, want 403", rr.Code)
	}
}

func TestAdminPassChangeInvalidatesOtherSessions(t *testing.T) {
	srv, _ := newTestServer(t)
	setAdminPass(t, srv, "correct-horse-battery")
	h := srv.Handler()
	prefix := "/" + testAdminSecret

	c1 := adminLogin(t, h, prefix, "correct-horse-battery")
	c2 := adminLogin(t, h, prefix, "correct-horse-battery")

	// Change the password from session 1.
	rr := doReq(t, h, http.MethodPost, prefix+"/admin-pass", url.Values{
		"new_password":     {"new-long-password-123"},
		"confirm_password": {"new-long-password-123"},
	}, c1)
	if rr.Code != http.StatusFound {
		t.Fatalf("admin-pass = %d, want 302 (body %s)", rr.Code, rr.Body.String())
	}

	// Session 2 (other admin) is invalidated: overview redirects to login.
	rr = doReq(t, h, http.MethodGet, prefix, nil, c2)
	if rr.Code != http.StatusFound {
		t.Fatalf("other session overview = %d, want 302 redirect", rr.Code)
	}
	// Session 1 (the changer) survives.
	rr = doReq(t, h, http.MethodGet, prefix, nil, c1)
	if rr.Code != http.StatusOK {
		t.Fatalf("changer session overview = %d, want 200", rr.Code)
	}
}

func TestBlockedDomainsSavePartial(t *testing.T) {
	srv, _ := newTestServer(t)
	setAdminPass(t, srv, "correct-horse-battery")
	h := srv.Handler()
	prefix := "/" + testAdminSecret
	cookie := adminLogin(t, h, prefix, "correct-horse-battery")

	// A mixed list: line 1 and 4 are invalid, the rest are saved.
	rr := doReqOrigin(t, h, http.MethodPost, prefix+"/blocked-domains",
		url.Values{"blocked": {"example.co.uk\nbad_domain\nok.example.com\nbad.com-"}},
		cookie, "https://"+testHost)
	if rr.Code != http.StatusFound {
		t.Fatalf("blocked-domains save = %d, want 302", rr.Code)
	}
	got, err := srv.db.GetBlockedDomains()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"example.co.uk", "ok.example.com"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("saved list = %v, want %v", got, want)
	}

	// The flash banner names the skipped line numbers (2, 4).
	rr = doReq(t, h, http.MethodPost, prefix+"/flash", nil, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("flash = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "2, 4") {
		t.Errorf("flash does not name the bad lines: %s", rr.Body.String())
	}

	// The domains page pre-fills the textarea with the saved list.
	rr = doReq(t, h, http.MethodGet, prefix+"/domains", nil, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /domains = %d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "example.co.uk") || !strings.Contains(rr.Body.String(), "Blocked domains") {
		t.Errorf("domains page missing blocked list textarea: %s", rr.Body.String())
	}
}

func TestBlockedDomainsSaveAllValid(t *testing.T) {
	srv, _ := newTestServer(t)
	setAdminPass(t, srv, "correct-horse-battery")
	h := srv.Handler()
	prefix := "/" + testAdminSecret
	cookie := adminLogin(t, h, prefix, "correct-horse-battery")

	rr := doReqOrigin(t, h, http.MethodPost, prefix+"/blocked-domains",
		url.Values{"blocked": {"a.com\nb.com"}}, cookie, "https://"+testHost)
	if rr.Code != http.StatusFound {
		t.Fatalf("save = %d, want 302", rr.Code)
	}
	got, err := srv.db.GetBlockedDomains()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("saved list = %v", got)
	}
	// No skip-banner when every line is valid.
	rr = doReq(t, h, http.MethodPost, prefix+"/flash", nil, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("flash = %d", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "skipped") {
		t.Errorf("unexpected skip banner: %s", rr.Body.String())
	}
}

// TestAdminKeysSave exercises the admin SSH-key management endpoint: an
// authenticated JSON save persists the operator's key store (comment stripped,
// name fallback), and an invalid key fails without changing stored keys.
func TestAdminKeysSave(t *testing.T) {
	srv, _ := newTestServer(t)
	setAdminPass(t, srv, "correct-horse-battery")
	h := srv.Handler()
	prefix := "/" + testAdminSecret
	cookie := adminLogin(t, h, prefix, "correct-horse-battery")

	// Save a key with a comment and no name: stored clean, name from comment.
	body := `{"keys":[{"id":0,"name":"","key":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBODYadminkeybogusbogusX ops-host","active":true}]}`
	req := httptest.NewRequest(http.MethodPost, prefix+"/keys", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("save = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Keys  []struct {
			ID     int64  `json:"id"`
			Name   string `json:"name"`
			Key    string `json:"key"`
			Active bool   `json:"active"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, rr.Body.String())
	}
	if !resp.OK {
		t.Fatalf("ok = false, error = %q", resp.Error)
	}
	if len(resp.Keys) != 1 {
		t.Fatalf("keys = %d, want 1", len(resp.Keys))
	}
	if resp.Keys[0].Name != "ops-host" {
		t.Errorf("name = %q, want comment fallback", resp.Keys[0].Name)
	}
	if strings.Contains(resp.Keys[0].Key, "ops-host") {
		t.Errorf("key not cleaned: %q", resp.Keys[0].Key)
	}
	if resp.Keys[0].ID == 0 {
		t.Error("new key should carry a real id")
	}

	// Unauthenticated POST is redirected, not saved.
	rr = doReq(t, h, http.MethodPost, prefix+"/keys", nil, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("unauthenticated save = %d, want 302", rr.Code)
	}

	// An invalid key fails the save and changes nothing.
	body = `{"keys":[{"id":0,"name":"bad","key":"not-a-key","active":true}]}`
	req = httptest.NewRequest(http.MethodPost, prefix+"/keys", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK {
		t.Error("invalid key should fail the save")
	}
	keys, err := srv.db.ListAdminKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("invalid save changed stored keys: %d", len(keys))
	}

	// The overview page renders the key row for the management panel.
	rr = doReq(t, h, http.MethodGet, prefix+"/", nil, cookie)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "keysModal") {
		t.Fatalf("overview missing key modal (code %d)", rr.Code)
	}
	// Admin keys have no per-key "active" checkbox anymore — every saved key is
	// visible to users. The CSS references kact, so match the rendered form.
	if strings.Contains(rr.Body.String(), `class="kact"`) {
		t.Error("admin key rows should no longer render an active checkbox")
	}
}

// TestLoginAs covers "log in as user": the admin POSTs /login-as and gets a
// redirect to the user panel with a fresh, impersonated user-panel session
// cookie, and the event is audited "000+<user>".
func TestLoginAs(t *testing.T) {
	srv, d := newTestServer(t)
	setAdminPass(t, srv, "correct-horse-battery")
	h := srv.Handler()
	prefix := "/" + testAdminSecret
	userPrefix := "/UserSecRet99" // cfg.Panel.URLPath in newTestServer
	if _, err := d.CreateUser("alice", "x", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	cookie := adminLogin(t, h, prefix, "correct-horse-battery")

	rr := doReq(t, h, http.MethodPost, prefix+"/login-as", url.Values{"name": {"alice"}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("login-as = %d, want 302 (body %s)", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != userPrefix+"/" {
		t.Fatalf("Location = %q, want %q", loc, userPrefix+"/")
	}
	var uc *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "vpsmgr_session" {
			v := *c
			uc = &v
		}
	}
	if uc == nil {
		t.Fatal("no user-panel session cookie set")
	}
	if uc.Path != userPrefix {
		t.Errorf("cookie Path = %q, want %q", uc.Path, userPrefix)
	}
	u, imp, err := d.SessionWithFlag(uc.Value)
	if err != nil {
		t.Fatalf("session lookup: %v", err)
	}
	if u.Name != "alice" || !imp {
		t.Errorf("session = user %q imp=%v, want alice imp=true", u.Name, imp)
	}
	rows, _ := d.ListAuditLog(0, 10)
	var found bool
	for _, r := range rows {
		if r.Actor == "000+alice" && r.Action == "session.login" {
			found = true
		}
	}
	if !found {
		t.Errorf("audit rows = %+v, want 000+alice session.login", rows)
	}
}

// TestLoginAsRequiresAdminAuth ensures /login-as is gated on an admin session.
func TestLoginAsRequiresAdminAuth(t *testing.T) {
	srv, _ := newTestServer(t)
	setAdminPass(t, srv, "correct-horse-battery")
	h := srv.Handler()
	prefix := "/" + testAdminSecret
	rr := doReq(t, h, http.MethodPost, prefix+"/login-as", url.Values{"name": {"alice"}}, nil)
	if rr.Code != http.StatusFound || rr.Header().Get("Location") != prefix+"/login" {
		t.Fatalf("unauthenticated login-as = %d (loc %q), want redirect to login", rr.Code, rr.Header().Get("Location"))
	}
}

// TestLoginAsDisabledUserPanel guards the disabled-panel edge case: with no
// user URLPath, /login-as must refuse instead of setting a Path="/" cookie.
func TestLoginAsDisabledUserPanel(t *testing.T) {
	srv, d := newTestServer(t)
	setAdminPass(t, srv, "correct-horse-battery")
	srv.cfg.Panel.URLPath = "" // user panel disabled
	h := srv.Handler()
	prefix := "/" + testAdminSecret
	if _, err := d.CreateUser("alice", "x", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	cookie := adminLogin(t, h, prefix, "correct-horse-battery")
	rr := doReq(t, h, http.MethodPost, prefix+"/login-as", url.Values{"name": {"alice"}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("login-as = %d, want 302", rr.Code)
	}
	for _, c := range rr.Result().Cookies() {
		if c.Name == "vpsmgr_session" {
			t.Error("must not set a user-panel cookie when the user panel is disabled")
		}
	}
}

// TestFlashCarriesData verifies the post-create flash delivers the username as
// opaque data so the modal can offer "log in as this user".
func TestFlashCarriesData(t *testing.T) {
	srv, _ := newTestServer(t)
	setAdminPass(t, srv, "correct-horse-battery")
	h := srv.Handler()
	prefix := "/" + testAdminSecret
	cookie := adminLogin(t, h, prefix, "correct-horse-battery")
	srv.flash.SetWithData(cookie.Value, "user:      alice\npassword:  xyz", "user_created", "alice")
	rr := doReq(t, h, http.MethodPost, prefix+"/flash", nil, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("/flash = %d", rr.Code)
	}
	b := rr.Body.String()
	for _, want := range []string{
		`"kind":"user_created"`,
		`"data":"alice"`,
		"\"msg\":\"user:      alice\\npassword:  xyz\"",
	} {
		if !strings.Contains(b, want) {
			t.Errorf("flash missing %s:\n%s", want, b)
		}
	}
}

// TestAdminLoginWritesAudit verifies admin login is audited as "000".
func TestAdminLoginWritesAudit(t *testing.T) {
	srv, d := newTestServer(t)
	setAdminPass(t, srv, "correct-horse-battery")
	h := srv.Handler()
	prefix := "/" + testAdminSecret
	adminLogin(t, h, prefix, "correct-horse-battery")
	rows, _ := d.ListAuditLog(0, 10)
	var found bool
	for _, r := range rows {
		if r.Actor == "000" && r.Action == "session.login" {
			found = true
		}
	}
	if !found {
		t.Errorf("audit rows = %+v, want 000 session.login", rows)
	}
}

// TestAdminOverviewLoginButtons checks the admin overview renders both the
// per-user "login panel" button and the modal's "log in as this user" form.
func TestAdminOverviewLoginButtons(t *testing.T) {
	srv, _ := newTestServer(t)
	html := srv.renderToString(t, "admin_overview.html", pageData{
		Prefix: "/" + testAdminSecret,
		Lang:   langZh,
		Users:  []userView{{Name: "alice", State: "Running", Status: "ready", SSHPort: "30001"}},
	})
	for _, want := range []string{
		"login-as", "登录面板", `name="name" value="alice"`,
		`id="loginAsForm"`, `id="loginAsName"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("admin overview missing %q", want)
		}
	}
}

// TestUserColorSet exercises the /user-color endpoint: a palette color
// persists, an out-of-palette value is rejected without changing the stored
// value, and an empty value resets the user to default. The event is audited
// "000+<user>".
func TestUserColorSet(t *testing.T) {
	srv, d := newTestServer(t)
	setAdminPass(t, srv, "correct-horse-battery")
	h := srv.Handler()
	prefix := "/" + testAdminSecret
	if _, err := d.CreateUser("alice", "x", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	cookie := adminLogin(t, h, prefix, "correct-horse-battery")

	// Valid palette color persists.
	rr := doReq(t, h, http.MethodPost, prefix+"/user-color",
		url.Values{"name": {"alice"}, "color": {"#16a34a"}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("user-color = %d, want 302 (body %s)", rr.Code, rr.Body.String())
	}
	u, err := d.GetUserByName("alice")
	if err != nil {
		t.Fatal(err)
	}
	if u.Color != "#16a34a" {
		t.Errorf("color = %q, want #16a34a", u.Color)
	}

	// Out-of-palette color is rejected and changes nothing.
	rr = doReq(t, h, http.MethodPost, prefix+"/user-color",
		url.Values{"name": {"alice"}, "color": {"#000000"}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("user-color invalid = %d, want 302", rr.Code)
	}
	u, _ = d.GetUserByName("alice")
	if u.Color != "#16a34a" {
		t.Errorf("invalid color changed stored value: %q", u.Color)
	}

	// Empty color resets to default.
	rr = doReq(t, h, http.MethodPost, prefix+"/user-color",
		url.Values{"name": {"alice"}, "color": {""}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("user-color reset = %d, want 302", rr.Code)
	}
	u, _ = d.GetUserByName("alice")
	if u.Color != "" {
		t.Errorf("color after reset = %q, want empty", u.Color)
	}

	rows, _ := d.ListAuditLog(0, 10)
	var found bool
	for _, r := range rows {
		if r.Actor == "000+alice" && r.Action == "color.update" {
			found = true
		}
	}
	if !found {
		t.Errorf("audit rows = %+v, want a 000+alice color.update row", rows)
	}
}

// TestAdminOverviewColorUI checks the per-user color UI in the overview:
// usernames render as bold clickable buttons (with a color dot + colored text
// when set), the color modal and full palette are present, and only a colored
// user's login button carries an inline theme color (the rest use the neutral
// default from the .login CSS rule).
func TestAdminOverviewColorUI(t *testing.T) {
	srv, _ := newTestServer(t)
	html := srv.renderToString(t, "admin_overview.html", pageData{
		Prefix: "/" + testAdminSecret,
		Lang:   langZh,
		Colors: userColorPalette,
		Users: []userView{
			{Name: "alice", State: "Running", Status: "ready", SSHPort: "30001", Color: "#16a34a"},
			{Name: "bob", State: "Running", Status: "ready", SSHPort: "30002"},
		},
	})
	for _, want := range []string{
		`class="uname"`, `onclick="pickColor('alice')"`, `onclick="pickColor('bob')"`,
		`class="udot" style="background:#16a34a"`, `style="color:#16a34a"`,
		`id="colorModal"`, `id="colorForm"`, `id="colorResetName"`,
		`name="color" value="#e11d48"`, `name="color" value="#3b82f6"`,
		`name="color" value="#c026d3"`, `name="color" value="#db2777"`,
		`style="--ubtn:#16a34a;color:#fff"`,
		`button.neutral { --ubtn: #1c1e21;`,         // neutral light default
		`class="primary neutral" id="cfBtn"`,        // create user: no blue
		`class="primary neutral" id="adminPassBtn"`, // change admin pw: no blue
	} {
		if !strings.Contains(html, want) {
			t.Errorf("admin overview missing %q", want)
		}
	}
	// Exactly 10 palette swatches render.
	if n := strings.Count(html, `type="submit" name="color"`); n != 10 {
		t.Errorf("palette swatches = %d, want 10", n)
	}
	// Only alice (the colored user) gets an inline --ubtn; bob falls back to
	// the neutral .login default.
	if n := strings.Count(html, `style="--ubtn:`); n != 1 {
		t.Errorf("inline --ubtn styles = %d, want exactly 1 (the colored user)", n)
	}
}

// renderToString executes a named admin template into a string for assertions.
func (s *Server) renderToString(t *testing.T, name string, data pageData) string {
	t.Helper()
	tpl, err := s.templates()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := tpl.ExecuteTemplate(&b, name, data); err != nil {
		t.Fatal(err)
	}
	return b.String()
}
