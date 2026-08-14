package panel

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
	"vpsmgr/internal/mgr"
	"vpsmgr/internal/pw"
)

const testSecret = "Ab1_cdE-9x"

func newTestServer(t *testing.T) (*Server, *db.DB) {
	t.Helper()
	c := cfg.Default()
	c.Panel.URLPath = testSecret
	c.Panel.PublicIP = "127.0.0.1"
	c.Panel.SessionDays = 3
	// Keep per-domain traefik writes out of /etc/traefik.
	t.Setenv("VPSMGR_TRAEFIK_DIR", t.TempDir())
	d, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return New(c, d, mgr.New(c, d)), d
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

// featureless asserts a bare 404: empty body, no Content-Type and no headers
// at all, identical for every wrong path so scanners learn nothing. Any header
// (including security headers) would fingerprint the service.
func featureless(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
	if body := rr.Body.String(); body != "" {
		t.Fatalf("body = %q, want empty", body)
	}
	if n := len(rr.Header()); n != 0 {
		t.Fatalf("headers = %v, want none (headers fingerprint the service)", rr.Header())
	}
}

func TestFeatureless404ForWrongPaths(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	paths := []string{
		"/",
		"/login",
		"/admin",
		"/robots.txt",
		"/favicon.ico",
		"/" + testSecret + "x", // near-miss prefix
		"/" + strings.ToUpper(testSecret),
		"//" + testSecret,
		"/secret/../" + testSecret,
		"/?q=1",
		"/anything/else",
	}
	// Every real route must be unreachable without the secret prefix.
	for _, route := range []string{"/login", "/logout", "/power", "/reinstall", "/password", "/root-reset", "/domain-add", "/domain-del", "/flash"} {
		paths = append(paths, route)
	}
	for _, p := range paths {
		rr := doReq(t, h, http.MethodGet, p, nil, nil)
		featureless(t, rr)
	}
	// POST too — scanners sending random POSTs must also get the bare 404.
	rr := doReq(t, h, http.MethodPost, "/"+testSecret+"x/login", url.Values{"username": {"root"}, "password": {"x"}}, nil)
	featureless(t, rr)
}

func TestPanelRoutesBehindPrefix(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testSecret

	// Prefix root requires auth: redirects to the prefixed login page.
	rr := doReq(t, h, http.MethodGet, prefix, nil, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("GET %s = %d, want redirect to login", prefix, rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != prefix+"/login" {
		t.Fatalf("Location = %q, want %q", loc, prefix+"/login")
	}
	// /login serves the login page.
	rr = doReq(t, h, http.MethodGet, prefix+"/login", nil, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "VPS Manager") {
		t.Fatalf("GET %s/login = %d, want login page", prefix, rr.Code)
	}
	// Login form posts to the prefixed action.
	if !strings.Contains(rr.Body.String(), prefix+"/login") {
		t.Fatalf("login form does not use prefixed action: %s", rr.Body.String())
	}
}

func TestLoginFlowAndCookiePath(t *testing.T) {
	srv, d := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testSecret

	hash, err := pw.Hash("correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateUser("alice", hash, "10.42.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}

	// Wrong password: login page re-rendered, no redirect.
	rr := doReq(t, h, http.MethodPost, prefix+"/login", url.Values{"username": {"alice"}, "password": {"nope"}}, nil)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "invalid credentials") {
		t.Fatalf("bad login: code=%d body=%s", rr.Code, rr.Body.String())
	}

	// Correct password: 302 to the prefix root and cookie scoped to prefix.
	rr = doReq(t, h, http.MethodPost, prefix+"/login", url.Values{"username": {"alice"}, "password": {"correct-horse-battery"}}, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("login = %d, want 302", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != prefix {
		t.Fatalf("Location = %q, want %q", loc, prefix)
	}
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "vpsmgr_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie set")
	}
	if cookie.Path != prefix {
		t.Fatalf("cookie Path = %q, want %q", cookie.Path, prefix)
	}

	// With the cookie, the prefixed root renders the overview page.
	rr = doReq(t, h, http.MethodGet, prefix, nil, cookie)
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "alice") {
		t.Fatalf("GET %s with session = %d, want overview", prefix, rr.Code)
	}
}

func TestUnknownSubpathUnderPrefix(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testSecret

	// Unknown routes under the known prefix fall through to the mux: not
	// authenticated, so they redirect to the login page (never a 404 leak).
	rr := doReq(t, h, http.MethodGet, prefix+"/admin", nil, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("GET %s/admin = %d, want redirect to login", prefix, rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != prefix+"/login" {
		t.Fatalf("Location = %q, want %q", loc, prefix+"/login")
	}
}

func TestOverviewShowsMonthlyBandwidth(t *testing.T) {
	srv, _ := newTestServer(t)
	html := srv.renderToString(t, "overview.html", pageData{
		User:   &db.User{Name: "alice"},
		UpGB:   "1.5",
		DownGB: "0.4",
		Prefix: "/" + testSecret,
	})
	// Unlimited: the bandwidth line lives in the Machine card and says so.
	for _, want := range []string{"Bandwidth", "unlimited", "↑ 1.5 / ↓ 0.4 GB"} {
		if !strings.Contains(html, want) {
			t.Errorf("overview (en default) missing %q", want)
		}
	}
	if strings.Contains(html, "本月流量") {
		t.Error("overview rendered Chinese without lang=zh")
	}
	// Explicit zh renders the Chinese labels.
	zh := srv.renderToString(t, "overview.html", pageData{
		User:   &db.User{Name: "alice"},
		UpGB:   "1.5",
		DownGB: "0.4",
		Prefix: "/" + testSecret,
		Lang:   langZh,
	})
	for _, want := range []string{"本月流量", "机器管理", "域名", "无限制"} {
		if !strings.Contains(zh, want) {
			t.Errorf("overview (zh) missing %q", want)
		}
	}
}

// TestOverviewDomSaveHiddenByDefault guards the domain save button: it must be
// hidden on load (only a PROXY checkbox change reveals it). The button carries
// the `hidden` attribute AND a .btn[hidden]{display:none} CSS rule — without
// the rule, .btn's display:inline-block would override the attribute and the
// button would always be visible.
func TestOverviewDomSaveHiddenByDefault(t *testing.T) {
	srv, _ := newTestServer(t)
	html := srv.renderToString(t, "overview.html", pageData{
		User:     &db.User{Name: "alice"},
		Prefix:   "/" + testSecret,
		Domains:  []domainRow{{Domain: "example.com", ProxyProtocol: false}},
		V4Forward: true,
	})
	if !strings.Contains(html, `id="domSave" hidden`) {
		t.Error("domSave button must be hidden by default")
	}
	if !strings.Contains(html, `.btn[hidden]{display:none}`) {
		t.Error("missing .btn[hidden]{display:none} CSS rule (hidden attribute is overridden by .btn display otherwise)")
	}
}

// TestOverviewConnectivityLayout covers the redesigned overview table: with v4
// forwarding on, the IPV4 row carries the public IP and the port block with a
// (?) help tooltip carrying the full range, and the SSH row shows both the V4
// DNAT port and the V6 port 22. With v4 forwarding off, the IPV4 row is gone
// and the V4 port shows "not available".
func TestOverviewConnectivityLayout(t *testing.T) {
	srv, _ := newTestServer(t)
	html := srv.renderToString(t, "overview.html", pageData{
		User:       &db.User{Name: "alice"},
		SSHPort:    30351,
		PublicIP:   "203.0.113.5",
		PortsShort: "103xx",
		Ports:      "10300-10399",
		IPv6:       "2a05:f480:1800:378d:0:8fa1:dddd:1",
		IPv6Block:  "2a05:f480:1800:378d:0:8fa1:dddd:0/112",
		V4Forward:  true,
		Prefix:     "/" + testSecret,
	})
	for _, want := range []string{"IPV4", "203.0.113.5", "103xx", "10300-10399", "30351", "V6 port", "22", "Address block"} {
		if !strings.Contains(html, want) {
			t.Errorf("overview (v4 on) missing %q", want)
		}
	}
	if !strings.Contains(html, `class="help"`) {
		t.Error("overview missing the (?) help icon on the port block")
	}

	off := srv.renderToString(t, "overview.html", pageData{
		User:      &db.User{Name: "alice"},
		SSHPort:   30351,
		IPv6:      "2a05:f480:1800:378d:0:8fa1:dddd:1",
		V4Forward: false,
		Prefix:    "/" + testSecret,
	})
	if strings.Contains(off, "203.0.113.5") || strings.Contains(off, "IPV4") {
		t.Error("v4-off overview should hide the IPV4 row")
	}
	if strings.Contains(off, "30351") {
		t.Error("v4-off overview should not show the v4 SSH port")
	}
	for _, want := range []string{"not available", "V4 port", "V6 port", "22"} {
		if !strings.Contains(off, want) {
			t.Errorf("v4-off overview missing %q", want)
		}
	}
}

// TestOverviewShowsBandwidthQuota verifies a limited user gets a quota progress
// bar, and an over-quota (throttled) user the "limited to 1Mbps" badge.
func TestOverviewShowsBandwidthQuota(t *testing.T) {
	srv, _ := newTestServer(t)
	html := srv.renderToString(t, "overview.html", pageData{
		User:           &db.User{Name: "alice"},
		UpGB:           "3.0",
		DownGB:         "1.0",
		Prefix:         "/" + testSecret,
		BandwidthQuotaGB: 100,
		BandwidthUsedGB:  "4.0",
		BandwidthPct:     4,
	})
	for _, want := range []string{"4.0 / 100 GB", "width:4%"} {
		if !strings.Contains(html, want) {
			t.Errorf("limited overview missing %q", want)
		}
	}
	if strings.Contains(html, "unlimited") {
		t.Error("limited overview should not say unlimited")
	}
	// Over quota: the throttled badge appears.
	throttled := srv.renderToString(t, "overview.html", pageData{
		User:           &db.User{Name: "alice"},
		UpGB:           "60.0",
		DownGB:         "41.0",
		Prefix:         "/" + testSecret,
		BandwidthQuotaGB: 100,
		BandwidthUsedGB:  "101.0",
		BandwidthPct:     100,
		Throttled:      true,
	})
	for _, want := range []string{"limited to 1Mbps", "width:100%"} {
		if !strings.Contains(throttled, want) {
			t.Errorf("throttled overview missing %q", want)
		}
	}
}

// renderToString executes a named template into a string for assertions.
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

// TestFlashViaAPI verifies result banners are stored server-side and fetched
// via a JSON endpoint (never in the URL), and that the password modal variant
// works the same way.
func TestFlashViaAPI(t *testing.T) {
	srv, d := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testSecret

	hash, err := pw.Hash("correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateUser("alice", hash, "10.42.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}

	rr := doReq(t, h, http.MethodPost, prefix+"/login", url.Values{"username": {"alice"}, "password": {"correct-horse-battery"}}, nil)
	var sess *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "vpsmgr_session" {
			v := *c
			sess = &v
		}
	}
	if sess == nil {
		t.Fatal("no session cookie")
	}

	// A state-changing POST with a mismatched password confirm redirects to the
	// prefix root WITHOUT leaking the message into the URL (deterministic, no
	// incus involvement).
	rr = doReq(t, h, http.MethodPost, prefix+"/password",
		url.Values{"new_password": {"xxxxxxxxxxxxxxxx"}, "confirm_password": {"yyyyyyyyyyyyyyyy"}}, sess)
	if rr.Code != http.StatusFound {
		t.Fatalf("password = %d, want 302", rr.Code)
	}
	if loc := rr.Header().Get("Location"); loc != prefix {
		t.Fatalf("Location = %q, want %q (message must not be a query param)", loc, prefix)
	}

	// The flash is exposed via /flash as JSON and consumed once.
	rr = doReq(t, h, http.MethodPost, prefix+"/flash", nil, sess)
	if rr.Code != http.StatusOK {
		t.Fatalf("/flash = %d, want 200", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "The two passwords do not match") {
		t.Fatalf("/flash body = %q, want the stored message", rr.Body.String())
	}
	rr = doReq(t, h, http.MethodPost, prefix+"/flash", nil, sess)
	if !strings.Contains(rr.Body.String(), `"msg":""`) {
		t.Fatalf("/flash after consume = %q, want empty", rr.Body.String())
	}

	// /flash is behind auth.
	rr = doReq(t, h, http.MethodPost, prefix+"/flash", nil, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("/flash without session = %d, want redirect to login", rr.Code)
	}
}

// TestPasswordModalFlash verifies the modal flash kind (root password / reinstall
// result) is delivered to the frontend.
func TestPasswordModalFlash(t *testing.T) {
	srv, d := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testSecret

	hash, err := pw.Hash("correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateUser("alice", hash, "10.42.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}

	// Log in to obtain a valid session token, then store a modal flash under it.
	rr := doReq(t, h, http.MethodPost, prefix+"/login", url.Values{"username": {"alice"}, "password": {"correct-horse-battery"}}, nil)
	var sess *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "vpsmgr_session" {
			v := *c
			sess = &v
		}
	}
	if sess == nil {
		t.Fatal("no session cookie")
	}
	srv.flash.Set(sess.Value, "新的 root 密码：\nAbcdefghijk1234567890", "modal")
	req := httptest.NewRequest(http.MethodPost, prefix+"/flash", nil)
	req.AddCookie(sess)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("/flash = %d, want 200", rw.Code)
	}
	if !strings.Contains(rw.Body.String(), `"kind":"modal"`) {
		t.Fatalf("/flash body = %q, want kind=modal", rw.Body.String())
	}
	if !strings.Contains(rw.Body.String(), "Abcdefghijk1234567890") {
		t.Fatalf("/flash body = %q, want the password", rw.Body.String())
	}
}

// TestLanguageSwitch verifies that browser language, the ?lang= param and the
// cookie pick the rendered language, and that an explicit toggle persists.
func TestLanguageSwitch(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testSecret

	// zh browser -> zh page.
	req := httptest.NewRequest(http.MethodGet, prefix+"/login", nil)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "VPS Manager 登录") {
		t.Fatalf("zh login page missing Chinese title")
	}

	// English browser (or no header) -> en page.
	rr = doReq(t, h, http.MethodGet, prefix+"/login", nil, nil)
	if !strings.Contains(rr.Body.String(), "VPS Manager Login") {
		t.Fatalf("default login page missing English title")
	}

	// Explicit ?lang=en on a zh browser wins and sets the cookie.
	req = httptest.NewRequest(http.MethodGet, prefix+"/login?lang=en", nil)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "VPS Manager Login") {
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
		t.Fatalf("?lang=en did not persist the vpsmgr_lang cookie")
	}

	// The cookie overrides the zh browser header.
	req = httptest.NewRequest(http.MethodGet, prefix+"/login", nil)
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")
	req.AddCookie(langCookieFound)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if !strings.Contains(rr.Body.String(), "VPS Manager Login") {
		t.Fatalf("cookie did not override browser language")
	}

	// lang="" normalizes to the English default rather than an unsupported value.
	if got := normalLang("fr-FR"); got != "" {
		t.Fatalf("normalLang(fr-FR) = %q, want \"\"", got)
	}
}

// TestTemplateEscapesUserInput verifies server-side rendering never emits raw
// markup from user-derived fields. The i18n/UI work renders the username and
// domain values in several new places (card headings, hidden inputs) — they must
// stay escaped first and last.
func TestTemplateEscapesUserInput(t *testing.T) {
	srv, _ := newTestServer(t)
	html := srv.renderToString(t, "overview.html", pageData{
		User:    &db.User{Name: `alice"><img src=x onerror=alert(1)>`},
		Domains: []domainRow{{Domain: `evil.example"><script>alert(1)</script>`}},
		UpGB:    "1",
		DownGB:  "1",
		Prefix:  "/" + testSecret,
	})
	for _, raw := range []string{`<img src=x onerror`, `<script>alert(1)</script>`, `"><script>`} {
		if strings.Contains(html, raw) {
			t.Fatalf("user input rendered unescaped: %q", raw)
		}
	}
	if !strings.Contains(html, "&lt;script&gt;alert(1)") && !strings.Contains(html, "\u0026lt;script") {
		t.Fatalf("expected an escaped script sequence in the page")
	}
}

func TestStripPrefix(t *testing.T) {
	const prefix = "/Ab1_cdE-9x"
	cases := []struct {
		path, rest string
		ok         bool
	}{
		{prefix, "/", true},
		{prefix + "/", "/", true},
		{prefix + "/login", "/login", true},
		{"/", "", false},
		{prefix + "x", "", false},
		{"//" + prefix, "", false},
	}
	for _, c := range cases {
		rest, ok := stripPrefix(c.path, prefix)
		if ok != c.ok || (ok && rest != c.rest) {
			t.Errorf("stripPrefix(%q) = (%q,%v), want (%q,%v)", c.path, rest, ok, c.rest, c.ok)
		}
	}
}

// TestImagesEndpoint verifies the lazy image-list endpoint: it always offers
// the default Debian image (Incus unreachable in tests falls back to it), and it
// is behind auth like every other panel route.
func TestImagesEndpoint(t *testing.T) {
	srv, d := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testSecret

	hash, err := pw.Hash("correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateUser("alice", hash, "10.42.0.2", 1, 30001, 10000, 10, 1024, 10); err != nil {
		t.Fatal(err)
	}
	rr := doReq(t, h, http.MethodPost, prefix+"/login", url.Values{"username": {"alice"}, "password": {"correct-horse-battery"}}, nil)
	var sess *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "vpsmgr_session" {
			v := *c
			sess = &v
		}
	}
	if sess == nil {
		t.Fatal("no session cookie")
	}

	rr = doReq(t, h, http.MethodPost, prefix+"/images", nil, sess)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /images = %d, want 200", rr.Code)
	}
	for _, want := range []string{"vpsmgr/debian-sshd", "Debian 13", `"default":"vpsmgr/debian-sshd"`} {
		if !strings.Contains(rr.Body.String(), want) {
			t.Errorf("/images body missing %q: %s", want, rr.Body.String())
		}
	}

	// Behind auth: no session -> redirect to login.
	rr = doReq(t, h, http.MethodPost, prefix+"/images", nil, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("POST /images without session = %d, want redirect", rr.Code)
	}
}

// TestWrongMethodOnPostOnlyRouteIsBare404 verifies that a non-POST request to a
// POST-only route (with a valid session) answers with the same bare 404 as any
// other wrong path — never a 405, which would advertise the POST-only endpoint.
func TestWrongMethodOnPostOnlyRouteIsBare404(t *testing.T) {
	srv, d := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testSecret

	hash, err := pw.Hash("correct-horse-battery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateUser("alice", hash, "10.42.0.2", 1, 30001, 10000, 10, 1024, 10); err != nil {
		t.Fatal(err)
	}
	rr := doReq(t, h, http.MethodPost, prefix+"/login", url.Values{"username": {"alice"}, "password": {"correct-horse-battery"}}, nil)
	var sess *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "vpsmgr_session" {
			v := *c
			sess = &v
		}
	}
	if sess == nil {
		t.Fatal("no session cookie")
	}

	rr = doReq(t, h, http.MethodGet, prefix+"/power", nil, sess)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("GET /power with session = %d, want 404", rr.Code)
	}
	if body := rr.Body.String(); body != "" {
		t.Fatalf("GET /power body = %q, want empty", body)
	}
	if hdr := rr.Header().Get("Allow"); hdr != "" {
		t.Fatalf("GET /power sets Allow header = %q, want none", hdr)
	}
}

func TestInitScriptSave(t *testing.T) {
	srv, d := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testSecret

	hash, err := pw.Hash("pw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateUser("alice", hash, "10.42.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}

	// Save without a session: redirected to login, nothing stored.
	rr := doReq(t, h, http.MethodPost, prefix+"/init-script", url.Values{"script": {"x"}}, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("unauthenticated save = %d, want 302", rr.Code)
	}

	// Login and grab the session cookie.
	rr = doReq(t, h, http.MethodPost, prefix+"/login", url.Values{"username": {"alice"}, "password": {"pw"}}, nil)
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "vpsmgr_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie")
	}

	// Save a normal script.
	script := "#!/bin/bash\napt-get update && apt-get install -y nginx"
	rr = doReq(t, h, http.MethodPost, prefix+"/init-script", url.Values{"script": {script}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("save = %d, want 302", rr.Code)
	}
	got, err := d.GetUserByName("alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.InitScript != script {
		t.Errorf("init_script = %q, want %q", got.InitScript, script)
	}

	// Oversize script is rejected and must not overwrite the stored value.
	big := strings.Repeat("x", cfg.MaxInitScriptBytes+1)
	rr = doReq(t, h, http.MethodPost, prefix+"/init-script", url.Values{"script": {big}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("oversize save = %d, want 302 (flash error)", rr.Code)
	}
	got, err = d.GetUserByName("alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.InitScript != script {
		t.Errorf("oversize save overwrote init_script: %q", got.InitScript)
	}

	// Clearing works.
	rr = doReq(t, h, http.MethodPost, prefix+"/init-script", url.Values{"script": {""}}, cookie)
	got, _ = d.GetUserByName("alice")
	if got.InitScript != "" {
		t.Errorf("clearing init_script failed: %q", got.InitScript)
	}
}

// loginAndCookie logs in as the user and returns the session cookie.
func loginAndCookie(t *testing.T, h http.Handler, prefix, name, pass string) *http.Cookie {
	t.Helper()
	rr := doReq(t, h, http.MethodPost, prefix+"/login", url.Values{"username": {name}, "password": {pass}}, nil)
	for _, c := range rr.Result().Cookies() {
		if c.Name == "vpsmgr_session" {
			return c
		}
	}
	t.Fatal("no session cookie on login")
	return nil
}

func TestDomainAddWithProxyProtocol(t *testing.T) {
	srv, d := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testSecret
	hash, _ := pw.Hash("pw")
	if _, err := d.CreateUser("alice", hash, "10.42.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	cookie := loginAndCookie(t, h, prefix, "alice", "pw")

	// With the proxy_protocol checkbox on.
	rr := doReq(t, h, http.MethodPost, prefix+"/domain-add",
		url.Values{"domain": {"api.example.com"}, "proxy_protocol": {"1"}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("add = %d, want 302", rr.Code)
	}
	dmn, err := d.GetDomainByDomain("api.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !dmn.ProxyProtocol {
		t.Error("proxy_protocol flag not stored")
	}

	// Without the checkbox: default off.
	rr = doReq(t, h, http.MethodPost, prefix+"/domain-add",
		url.Values{"domain": {"plain.example.com"}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("add = %d, want 302", rr.Code)
	}
	plain, _ := d.GetDomainByDomain("plain.example.com")
	if plain.ProxyProtocol {
		t.Error("proxy_protocol should default off")
	}
}

func TestDomainUpdateBatch(t *testing.T) {
	srv, d := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testSecret
	hash, _ := pw.Hash("pw")
	u, err := d.CreateUser("alice", hash, "10.42.0.2", 1, 30001, 10000, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddDomain(u.ID, "example.com", false); err != nil {
		t.Fatal(err)
	}
	cookie := loginAndCookie(t, h, prefix, "alice", "pw")

	// Post the checkbox for example.com (proto[] present = on).
	rr := doReq(t, h, http.MethodPost, prefix+"/domain-update",
		url.Values{"proto": {"example.com"}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("update = %d, want 302", rr.Code)
	}
	dmn, err := d.GetDomainByDomain("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !dmn.ProxyProtocol {
		t.Error("batch update did not enable proxy protocol")
	}

	// Post with no proto[] = toggle back off.
	rr = doReq(t, h, http.MethodPost, prefix+"/domain-update", url.Values{}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("update = %d, want 302", rr.Code)
	}
	dmn, _ = d.GetDomainByDomain("example.com")
	if dmn.ProxyProtocol {
		t.Error("batch update did not disable proxy protocol")
	}
}

func TestDomainAddLogsAudit(t *testing.T) {
	srv, d := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testSecret
	hash, _ := pw.Hash("pw")
	if _, err := d.CreateUser("alice", hash, "10.42.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	cookie := loginAndCookie(t, h, prefix, "alice", "pw")
	rr := doReq(t, h, http.MethodPost, prefix+"/domain-add",
		url.Values{"domain": {"example.com"}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("add = %d, want 302", rr.Code)
	}
	rows, err := d.ListAuditLog(0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Actor != "alice" || rows[0].Action != "domain_update" {
		t.Errorf("audit rows = %+v", rows)
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

func TestPanelCSRFRejectsCrossOriginPOST(t *testing.T) {
	srv, d := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testSecret
	hash, _ := pw.Hash("pw")
	if _, err := d.CreateUser("alice", hash, "10.42.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	cookie := loginAndCookie(t, h, prefix, "alice", "pw")

	// Cross-origin state-changing POST rejected before the handler runs.
	rr := doReqOrigin(t, h, http.MethodPost, prefix+"/power",
		url.Values{"action": {"restart"}}, cookie, "https://evil.example.com")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin POST = %d, want 403", rr.Code)
	}

	// Cross-origin login POST rejected too (login CSRF needs no session).
	rr = doReqOrigin(t, h, http.MethodPost, prefix+"/login",
		url.Values{"username": {"alice"}, "password": {"pw"}}, nil, "https://evil.example.com")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("cross-origin login POST = %d, want 403", rr.Code)
	}

	// Same-origin POST is allowed through (no CSRF block).
	rr = doReqOrigin(t, h, http.MethodPost, prefix+"/power",
		url.Values{"action": {"restart"}}, cookie, "https://example.com")
	if rr.Code == http.StatusForbidden {
		t.Fatal("same-origin POST must not be blocked by CSRF")
	}
}
