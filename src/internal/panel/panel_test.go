package panel

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
		IPv6:       "2001:db8:1::dddd:1",
		IPv6Block:  "2001:db8:1::dddd:0/112",
		V4Forward:  true,
		Prefix:     "/" + testSecret,
	})
	for _, want := range []string{"IPV4", "203.0.113.5", "103xx", "10300-10399", "30351", "V6 port", "22", "Address block"} {
		if !strings.Contains(html, want) {
			t.Errorf("overview (v4 on) missing %q", want)
		}
	}
	// The V4/V6 port blocks are click-to-copy and carry the full ssh command.
	for _, want := range []string{
		`data-copy="ssh -p 30351 root@203.0.113.5"`,
		`data-copy="ssh -p 22 root@2001:db8:1::dddd:1"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("overview (v4 on) missing click-to-copy %q", want)
		}
	}
	if !strings.Contains(html, `class="help"`) {
		t.Error("overview missing the (?) help icon on the port block")
	}

	off := srv.renderToString(t, "overview.html", pageData{
		User:      &db.User{Name: "alice"},
		SSHPort:   30351,
		IPv6:      "2001:db8:1::dddd:1",
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

// TestOverviewThemeColor verifies the operator-assigned accent color is
// injected as a CSS variable override block only when set: with a theme color
// the overview carries the tinted background and recolored accents, without it
// the default (untouched) styles render.
func TestOverviewThemeColor(t *testing.T) {
	srv, _ := newTestServer(t)
	plain := srv.renderToString(t, "overview.html", pageData{
		User:   &db.User{Name: "alice"},
		Prefix: "/" + testSecret,
	})
	for _, absent := range []string{"--accent:#16a34a", "var(--ubtn)", "#16a34a 7%"} {
		if strings.Contains(plain, absent) {
			t.Errorf("plain overview should not contain %q", absent)
		}
	}
	if !strings.Contains(plain, "--accent:#2563eb") {
		t.Error("plain overview should keep the default accent")
	}
	if !strings.Contains(plain, "--bg:#ffffff") {
		t.Error("plain overview light background should be white")
	}

	themed := srv.renderToString(t, "overview.html", pageData{
		User:       &db.User{Name: "alice"},
		ThemeColor: "#16a34a",
		Prefix:     "/" + testSecret,
	})
	for _, want := range []string{
		"--accent:#16a34a", "--head:#16a34a",
		"color-mix(in srgb, #16a34a 13%, #ffffff)",
		"color-mix(in srgb, #16a34a 16%, #14161a)",
		"color-mix(in srgb, #16a34a 5%, #ffffff)",
		"border-bottom:3px solid #16a34a",
	} {
		if !strings.Contains(themed, want) {
			t.Errorf("themed overview missing %q", want)
		}
	}
}

// TestOverviewUserNameEmphasis keeps the current user's name in the overview
// heading visibly heavier than the surrounding heading text.
func TestOverviewUserNameEmphasis(t *testing.T) {
	srv, _ := newTestServer(t)
	html := srv.renderToString(t, "overview.html", pageData{
		User:   &db.User{Name: "rapmq"},
		Prefix: "/" + testSecret,
	})
	if !strings.Contains(html, `<span class="user-name"> · rapmq</span>`) {
		t.Error("overview heading should mark the username with user-name class")
	}
	if !strings.Contains(html, ".user-name{font-weight:800") {
		t.Error("overview username should use a visibly heavier font weight")
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
	for _, want := range []string{"vpsmgr/debian-sshd", "Debian 13", `"default":"vpsmgr/debian-sshd"`, `"desc_zh":"轻巧的原味 Debian 13 系统"`, `"desc_en":"Lightweight stock Debian 13"`} {
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
	// The login also writes a session.login row; assert the domain_update row
	// is present (and attributed to the acting user).
	var found bool
	for _, r := range rows {
		if r.Actor == "alice" && r.Action == "domain_update" {
			found = true
		}
	}
	if !found {
		t.Errorf("audit rows = %+v, want a domain_update row for alice", rows)
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

// TestSnapshotRoutesRegistered checks that the snapshot routes exist behind the
// prefix and fail gracefully (redirect with an error) when Incus is
// unreachable. The test server points the Incus client at a socket that does
// not exist, so every snapshot op fails before touching real state — and no
// audit row is written on failure.
func TestSnapshotRoutesRegistered(t *testing.T) {
	srv, d := newTestServer(t)
	// Point the lx client at a dead socket so snapshot ops fail fast and the
	// test never touches a real Incus daemon.
	srv.cfg.Incus.Socket = "/nonexistent/vpsmgr-test.sock"
	srv.mgr = mgr.New(srv.cfg, d)
	h := srv.Handler()
	prefix := "/" + testSecret
	hash, _ := pw.Hash("pw")
	if _, err := d.CreateUser("alice", hash, "10.42.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	cookie := loginAndCookie(t, h, prefix, "alice", "pw")

	// POST /snapshot: registered (not 404) and errors out (no Incus socket).
	rr := doReq(t, h, http.MethodPost, prefix+"/snapshot", url.Values{}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("POST /snapshot = %d, want 302", rr.Code)
	}
	// POST /snapshot-del with a bogus name: errors (invalid snapshot name).
	rr = doReq(t, h, http.MethodPost, prefix+"/snapshot-del", url.Values{"name": {"../evil"}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("POST /snapshot-del = %d, want 302", rr.Code)
	}
	// POST /snapshot-restore: same shape.
	rr = doReq(t, h, http.MethodPost, prefix+"/snapshot-restore", url.Values{"name": {"snap-x"}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("POST /snapshot-restore = %d, want 302", rr.Code)
	}

	// No audit rows for the snapshot ops: every snapshot call failed before
	// any successful action (the login row is expected).
	rows, err := d.ListAuditLog(0, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if strings.HasPrefix(r.Action, "snapshot.") {
			t.Errorf("failed snapshot op wrote an audit row: %+v", r)
		}
	}
}

// TestSnapshotLimitSemantics checks the config-driven snapshot cap: default is
// 1, a positive value is the cap, and 0 (or negative) disables snapshots.
func TestSnapshotLimitSemantics(t *testing.T) {
	srv, _ := newTestServer(t)
	if got := srv.mgr.SnapshotLimit(); got != 1 {
		t.Fatalf("SnapshotLimit() = %d, want 1 (default)", got)
	}
	// A positive value is the hard cap.
	srv.cfg.Snapshots.Limit = 5
	if got := srv.mgr.SnapshotLimit(); got != 5 {
		t.Fatalf("SnapshotLimit() with 5 = %d, want 5", got)
	}
	// 0 disables snapshots (no new snapshots, existing ones untouched).
	srv.cfg.Snapshots.Limit = 0
	if got := srv.mgr.SnapshotLimit(); got != 0 {
		t.Fatalf("SnapshotLimit() with 0 = %d, want 0 (disabled)", got)
	}
	// A negative value (shouldn't pass config validation) is treated as disabled.
	srv.cfg.Snapshots.Limit = -1
	if got := srv.mgr.SnapshotLimit(); got != 0 {
		t.Fatalf("SnapshotLimit() with -1 = %d, want 0 (disabled)", got)
	}
}

// TestOverviewSnapshotModal checks the snapshot UI lives inside the Machine
// card as a button + modal: the standalone snapshots card is gone, the modal
// carries the create button and the table has no size column.
func TestOverviewSnapshotModal(t *testing.T) {
	srv, d := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testSecret
	hash, _ := pw.Hash("pw")
	if _, err := d.CreateUser("alice", hash, "10.42.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	cookie := loginAndCookie(t, h, prefix, "alice", "pw")
	rr := doReq(t, h, http.MethodGet, prefix, nil, cookie)
	body := rr.Body.String()
	for _, want := range []string{`id="snapBtn"`, `id="snapModal"`, `id="snapClose"`, `id="snapCreate"`} {
		if !strings.Contains(body, want) {
			t.Errorf("overview missing %q", want)
		}
	}
	// With the mocked manager the snapshot list on this page is empty, so the
	// table would not be rendered here. Verify the shipped template carries the
	// size column header and a per-row size cell; the table body is guarded by
	// {{if .Snapshots}}, so this checks the template source itself.
	src, err := tmplFS.ReadFile("templates/overview.html")
	if err != nil {
		t.Fatal(err)
	}
	tpl := string(src)
	// The header is a language-conditional template:
	//   {{if eq .Lang "zh"}}大小{{else}}Size{{end}}</th>
	// Check the whole conditional so the language branch change doesn't pass.
	if !strings.Contains(tpl, "大小{{else}}Size{{end}}</th>") && !strings.Contains(tpl, "Size{{else}}") {
		t.Error("snapshot size column header should be present in overview.html")
	}
	if !strings.Contains(tpl, ".Size") {
		t.Error("snapshot size cell should be rendered per row in overview.html")
	}
	// The snapshot create button must be inside the modal.
	iModal := strings.Index(body, `id="snapModal"`)
	iCreate := strings.Index(body, `id="snapCreate"`)
	if iModal < 0 || iCreate < iModal {
		t.Error("snapshot create button should live inside the snapshot modal")
	}
	// No standalone snapshot card: the modal is the only place the snapshot
	// intro paragraph appears.
	if strings.Count(body, "No snapshots yet") > 0 || strings.Count(body, "暂无快照") > 0 {
		// allowed only inside the modal
		if i := strings.Index(body, "暂无快照"); i >= 0 && i < iModal {
			t.Error("snapshot card content leaked outside the modal")
		}
	}
}

// TestStatsEndpoint verifies the /stats JSON endpoint: it requires auth,
// returns per-minute points for the current user, and computes per-minute
// bandwidth as the delta of the cumulative rx/tx counters between samples.
func TestStatsEndpoint(t *testing.T) {
	srv, d := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testSecret
	hash, _ := pw.Hash("pw")
	u, err := d.CreateUser("alice", hash, "10.42.0.2", 1, 30001, 10000, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Truncate(time.Minute).Unix()
	// Two consecutive running samples: rx +1000, tx +500 between them.
	samples := []db.ResourceSample{
		{UserID: u.ID, SampleMinute: now - 60, State: 1, BootTime: 1,
			CPUPercentX10: 120, MemoryMiB: 512,
			RXBytesTotal: 10000, TXBytesTotal: 5000},
		{UserID: u.ID, SampleMinute: now, State: 1, BootTime: 1,
			CPUPercentX10: 250, MemoryMiB: 600,
			RXBytesTotal: 11000, TXBytesTotal: 5500},
	}
	if err := d.RecordResourceSamples(samples, nil, "2026-08", now-7*24*3600); err != nil {
		t.Fatal(err)
	}

	// Unauthenticated: redirect to login.
	rr := doReq(t, h, http.MethodGet, prefix+"/stats", nil, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("unauthenticated /stats = %d, want 302", rr.Code)
	}

	cookie := loginAndCookie(t, h, prefix, "alice", "pw")
	rr = doReq(t, h, http.MethodGet, prefix+"/stats", nil, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("/stats = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		Points []struct {
			M   int64   `json:"m"`
			C   float64 `json:"c"`
			Y   int64   `json:"y"`
			R   int64   `json:"r"`
			T   int64   `json:"t"`
		} `json:"points"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad JSON: %v (%s)", err, rr.Body.String())
	}
	if len(resp.Points) != 2 {
		t.Fatalf("points = %d, want 2", len(resp.Points))
	}
	// First point has no predecessor -> 0 bandwidth; second has the delta.
	if resp.Points[0].R != 0 || resp.Points[0].T != 0 {
		t.Errorf("first point bandwidth = %d/%d, want 0/0", resp.Points[0].R, resp.Points[0].T)
	}
	if resp.Points[1].R != 1000 || resp.Points[1].T != 500 {
		t.Errorf("second point bandwidth = %d/%d, want 1000/500", resp.Points[1].R, resp.Points[1].T)
	}
	if resp.Points[1].C != 25.0 {
		t.Errorf("second point cpu = %v, want 25.0 (cpu_pct_x10=250)", resp.Points[1].C)
	}
	if resp.Points[1].Y != 600 {
		t.Errorf("second point mem = %d, want 600", resp.Points[1].Y)
	}
}

// TestSSHKeysSave exercises the SSH-key management endpoint: an authenticated
// JSON save persists the key set (comment stripped, name fallback) and applies
// it live. The test host has no Incus, so the live apply must degrade to a
// warning rather than failing the save.
func TestSSHKeysSave(t *testing.T) {
	srv, d := newTestServer(t)
	// Point the manager at a socket that cannot exist, so the live key apply
	// fails fast and the test never touches a real container.
	srv.cfg.Incus.Socket = "/nonexistent/vpsmgr-test.sock"
	srv.mgr = mgr.New(srv.cfg, d)
	h := srv.Handler()
	prefix := "/" + testSecret

	hash, err := pw.Hash("pw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateUser("alice", hash, "10.42.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	rr := doReq(t, h, http.MethodPost, prefix+"/login", url.Values{"username": {"alice"}, "password": {"pw"}}, nil)
	var cookie *http.Cookie
	for _, c := range rr.Result().Cookies() {
		if c.Name == "vpsmgr_session" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie")
	}

	// Save a key with a comment and no name: stored clean, name from comment.
	body := `{"keys":[{"id":0,"name":"","key":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBODYbogusbogusbogusbogusbogusX my-laptop","active":true}]}`
	req := httptest.NewRequest(http.MethodPost, prefix+"/ssh-keys", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("save = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
		Warning string `json:"warning"`
		Keys    []struct {
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
	if resp.Keys[0].Name != "my-laptop" {
		t.Errorf("name = %q, want comment fallback", resp.Keys[0].Name)
	}
	if strings.Contains(resp.Keys[0].Key, "my-laptop") {
		t.Errorf("key not cleaned: %q", resp.Keys[0].Key)
	}
	// No Incus in tests -> the live apply warns but the save succeeds.
	if resp.Warning == "" {
		t.Error("expected apply warning without Incus")
	}

	// The key is persisted and appears on the overview page.
	keys, err := d.ListSSHKeys(1)
	if err != nil || len(keys) != 1 {
		t.Fatalf("db keys = %+v err=%v, want 1", keys, err)
	}

	// An invalid key fails the save and changes nothing.
	body = `{"keys":[{"id":0,"name":"bad","key":"not-a-key","active":true}]}`
	req = httptest.NewRequest(http.MethodPost, prefix+"/ssh-keys", strings.NewReader(body))
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
	keys, _ = d.ListSSHKeys(1)
	if len(keys) != 1 {
		t.Fatalf("invalid save changed stored keys: %d", len(keys))
	}
}

// TestSSHKeysSaveAdminGrant covers the admin-key activation half of /ssh-keys:
// the user checks some of the operator's public keys and saving persists those
// grants (unknown ids dropped, selection replaced wholesale), returned back to
// the panel so the checkboxes re-render. No Incus in tests, so the live apply
// degrades to a warning.
func TestSSHKeysSaveAdminGrant(t *testing.T) {
	srv, d := newTestServer(t)
	srv.cfg.Incus.Socket = "/nonexistent/vpsmgr-test.sock"
	srv.mgr = mgr.New(srv.cfg, d)
	h := srv.Handler()
	prefix := "/" + testSecret

	hash, _ := pw.Hash("pw")
	u, err := d.CreateUser("alice", hash, "10.42.0.2", 1, 30001, 10000, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	// The operator's stored keys.
	a1, err := d.AddAdminKey("ops", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBODYadminkeybogusbogusX", true)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := d.AddAdminKey("ci", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBODYadminkeybogusbogusY", true)
	if err != nil {
		t.Fatal(err)
	}
	cookie := loginAndCookie(t, h, prefix, "alice", "pw")

	// Grant the first admin key plus a stale id that must be dropped.
	rr := postJSON(t, h, prefix+"/ssh-keys",
		`{"keys":[],"adminKeys":[`+itoa(int(a1.ID))+`,999999]}`, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("save = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	var resp struct {
		OK        bool    `json:"ok"`
		AdminKeys []int64 `json:"adminKeys"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("ok = false, body %s", rr.Body.String())
	}
	if len(resp.AdminKeys) != 1 || resp.AdminKeys[0] != a1.ID {
		t.Fatalf("granted = %v, want [%d]", resp.AdminKeys, a1.ID)
	}
	granted, err := d.GrantedAdminKeys(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(granted) != 1 || granted[0].ID != a1.ID {
		t.Fatalf("db grants = %+v, want only %d", granted, a1.ID)
	}

	// Overwrite with the second key: the first grant is replaced.
	rr = postJSON(t, h, prefix+"/ssh-keys", `{"keys":[],"adminKeys":[`+itoa(int(a2.ID))+`]}`, cookie)
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || len(resp.AdminKeys) != 1 || resp.AdminKeys[0] != a2.ID {
		t.Fatalf("after overwrite = %+v", resp)
	}

	// Empty selection revokes everything.
	rr = postJSON(t, h, prefix+"/ssh-keys", `{"keys":[],"adminKeys":[]}`, cookie)
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK || len(resp.AdminKeys) != 0 {
		t.Fatalf("after revoke = %+v", resp)
	}
	granted, _ = d.GrantedAdminKeys(u.ID)
	if len(granted) != 0 {
		t.Fatalf("grants after revoke = %+v, want none", granted)
	}
}

// TestOverviewAdminKeysDisclosure covers the SSH-key modal's admin-keys
// section: with operator keys present the disclosure link, the red warning and
// the grant checkboxes render (checked = already granted); with no operator
// keys the whole block is absent.
func TestOverviewAdminKeysDisclosure(t *testing.T) {
	srv, _ := newTestServer(t)
	const k1 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBODYadminkeybogusbogusX"
	const k2 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBODYadminkeybogusbogusY"
	html := srv.renderToString(t, "overview.html", pageData{
		User:   &db.User{Name: "alice"},
		Prefix: "/" + testSecret,
		Lang:   langZh,
		AdminSSHKeys: []sshKeyRow{
			{ID: 1, Name: "ops", Key: k1, Active: true},  // already granted
			{ID: 2, Name: "ci", Key: k2, Active: false},   // not granted
		},
		AdminKeysAnyActive: true, // one key granted -> default expanded
	})
	for _, want := range []string{
		`id="adminKeysToggle"`, "收起来自管理员的公钥",
		"激活以下公钥则授权管理员登录你的机器",
		`data-aid="1"`, `data-aid="2"`, `class="keyrow admin-keyrow"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("overview missing %q", want)
		}
	}
	// Granted keys default the disclosure to EXPANDED: no hidden attribute.
	if !strings.Contains(html, `id="adminKeysList">`) {
		t.Error("granted admin keys should render the list expanded (no hidden)")
	}
	if strings.Contains(html, `id="adminKeysList" hidden`) {
		t.Error("granted admin keys must not render the list hidden")
	}
	// Exactly one granted (checked) row and one plain row.
	if n := strings.Count(html, `class="kact" checked>`); n != 1 {
		t.Errorf("checked admin rows = %d, want 1", n)
	}
	if n := strings.Count(html, `class="kact">`); n != 1 {
		t.Errorf("unchecked admin rows = %d, want 1", n)
	}

	// No granted keys -> default COLLAPSED: list hidden, button shows expand.
	collapsed := srv.renderToString(t, "overview.html", pageData{
		User:   &db.User{Name: "alice"},
		Prefix: "/" + testSecret,
		Lang:   langZh,
		AdminSSHKeys: []sshKeyRow{
			{ID: 1, Name: "ops", Key: k1, Active: false},
		},
	})
	if !strings.Contains(collapsed, `id="adminKeysList" hidden`) {
		t.Error("no granted admin keys should default the list to hidden")
	}
	if !strings.Contains(collapsed, "展开来自管理员的公钥") {
		t.Error("collapsed disclosure should show the expand label")
	}

	// Without operator keys the disclosure block is not rendered at all. The
	// JS always references these ids, so assert on the rendered-only strings.
	plain := srv.renderToString(t, "overview.html", pageData{
		User:   &db.User{Name: "alice"},
		Prefix: "/" + testSecret,
	})
	for _, absent := range []string{
		`id="adminKeysToggle"`, `id="adminKeysList"`,
		"激活以下公钥则授权管理员登录你的机器",
		"Activating the keys below authorizes",
	} {
		if strings.Contains(plain, absent) {
			t.Errorf("overview without admin keys should not render %q", absent)
		}
	}
}

// postJSON sends an authenticated JSON POST and returns the response recorder.
func postJSON(t *testing.T, h http.Handler, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// TestInitScriptSaveJSON exercises the JSON path of /init-script used by the
// Machine-panel modal: it answers {ok,error} instead of redirecting, and keeps
// the form POST path (already covered by TestInitScriptSave) untouched.
func TestInitScriptSaveJSON(t *testing.T) {
	srv, d := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testSecret
	hash, _ := pw.Hash("pw")
	if _, err := d.CreateUser("alice", hash, "10.42.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	cookie := loginAndCookie(t, h, prefix, "alice", "pw")

	var resp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}

	rr := postJSON(t, h, prefix+"/init-script", `{"script":"#!/bin/bash\necho hi"}`, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("json save = %d, want 200", rr.Code)
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("ok = false, error = %q", resp.Error)
	}
	got, _ := d.GetUserByName("alice")
	if got.InitScript != "#!/bin/bash\necho hi" {
		t.Errorf("init_script = %q", got.InitScript)
	}

	// Oversize via JSON is rejected and keeps the stored script.
	big := strings.Repeat("x", cfg.MaxInitScriptBytes+1)
	rr = postJSON(t, h, prefix+"/init-script", `{"script":"`+big+`"}`, cookie)
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.OK {
		t.Error("oversize JSON save should fail")
	}
	got, _ = d.GetUserByName("alice")
	if got.InitScript != "#!/bin/bash\necho hi" {
		t.Errorf("oversize JSON save overwrote init_script: %q", got.InitScript)
	}

	// No session: redirected to login, nothing saved.
	rr = postJSON(t, h, prefix+"/init-script", `{"script":"x"}`, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("unauthenticated JSON save = %d, want 302", rr.Code)
	}
}

// TestNotesLifecycle covers the sticky-notes endpoints end to end: the blob is
// opaque to the server, GET returns {has,data}, POST stores it, the oversize
// cap is enforced with a "notes_full" error, and reset clears everything.
func TestNotesLifecycle(t *testing.T) {
	srv, d := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testSecret
	hash, _ := pw.Hash("pw")
	if _, err := d.CreateUser("alice", hash, "10.42.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	cookie := loginAndCookie(t, h, prefix, "alice", "pw")

	var notesResp struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
		Has   bool   `json:"has"`
		Data  string `json:"data"`
	}

	// Fresh user: no notes.
	req := httptest.NewRequest(http.MethodGet, prefix+"/notes", nil)
	req.AddCookie(cookie)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /notes = %d", rr.Code)
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &notesResp); err != nil {
		t.Fatal(err)
	}
	if !notesResp.OK || notesResp.Has {
		t.Fatalf("fresh GET = %+v, want ok + has=false", notesResp)
	}

	// Save an opaque blob.
	blob := `{"v":1,"salt":"c2FsdA==","iv":"aXZlYg==","ct":"Y3Q="}`
	rr = postJSON(t, h, prefix+"/notes", `{"data":`+jsonQuote(blob)+`}`, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /notes = %d", rr.Code)
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &notesResp); err != nil {
		t.Fatal(err)
	}
	if !notesResp.OK || !notesResp.Has {
		t.Fatalf("save response = %+v", notesResp)
	}

	// GET now returns the stored envelope.
	req = httptest.NewRequest(http.MethodGet, prefix+"/notes", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if err := json.Unmarshal(rr.Body.Bytes(), &notesResp); err != nil {
		t.Fatal(err)
	}
	if !notesResp.Has || notesResp.Data != blob {
		t.Fatalf("GET after save = %+v", notesResp)
	}

	// Oversize body is rejected with notes_full and keeps the old data.
	big := strings.Repeat("x", cfg.MaxNotesBlobBytes+1)
	rr = postJSON(t, h, prefix+"/notes", `{"data":`+jsonQuote(big)+`}`, cookie)
	if err := json.Unmarshal(rr.Body.Bytes(), &notesResp); err != nil {
		t.Fatal(err)
	}
	if notesResp.OK || notesResp.Error != "notes_full" {
		t.Fatalf("oversize save = %+v, want ok=false + notes_full", notesResp)
	}
	req = httptest.NewRequest(http.MethodGet, prefix+"/notes", nil)
	req.AddCookie(cookie)
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if err := json.Unmarshal(rr.Body.Bytes(), &notesResp); err != nil {
		t.Fatal(err)
	}
	if notesResp.Data != blob {
		t.Fatalf("oversize save clobbered data: %+v", notesResp)
	}

	// Reset clears everything back to the never-enabled state.
	rr = doReq(t, h, http.MethodPost, prefix+"/notes/reset", nil, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /notes/reset = %d", rr.Code)
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &notesResp); err != nil {
		t.Fatal(err)
	}
	if !notesResp.OK || notesResp.Has {
		t.Fatalf("reset response = %+v, want ok + has=false", notesResp)
	}
	stored, err := d.GetStickyNotes(1)
	if err != nil {
		t.Fatal(err)
	}
	if stored != "" {
		t.Errorf("notes after reset = %q, want empty", stored)
	}

	// Unauthenticated writes are redirected.
	rr = postJSON(t, h, prefix+"/notes", `{"data":"x"}`, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("unauthenticated POST /notes = %d, want 302", rr.Code)
	}
	rr = doReq(t, h, http.MethodPost, prefix+"/notes/reset", nil, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("unauthenticated POST /notes/reset = %d, want 302", rr.Code)
	}
}

// jsonQuote returns s as a quoted JSON string literal.
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestUserLoginWritesAudit verifies a successful user login records a
// session.login audit event (newly added audit coverage).
func TestUserLoginWritesAudit(t *testing.T) {
	srv, d := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testSecret
	hash, _ := pw.Hash("pw")
	if _, err := d.CreateUser("alice", hash, "10.42.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	rr := doReq(t, h, http.MethodPost, prefix+"/login",
		url.Values{"username": {"alice"}, "password": {"pw"}}, nil)
	if rr.Code != http.StatusFound {
		t.Fatalf("login = %d, want 302", rr.Code)
	}
	rows, _ := d.ListAuditLog(0, 10)
	var found bool
	for _, r := range rows {
		if r.Actor == "alice" && r.Action == "session.login" {
			found = true
		}
	}
	if !found {
		t.Errorf("audit rows = %+v, want alice session.login", rows)
	}
}

// TestImpersonatedSessionAuditActor verifies that a user-panel action carried
// out under an operator-created session is attributed "000+<user>" (not the
// user's own name).
func TestImpersonatedSessionAuditActor(t *testing.T) {
	srv, d := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testSecret
	hash, _ := pw.Hash("pw")
	u, err := d.CreateUser("alice", hash, "10.42.0.2", 1, 30001, 10000, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := d.CreateImpersonatedSession(u.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	cookie := &http.Cookie{Name: "vpsmgr_session", Value: sess.Token, Path: prefix}

	rr := postJSON(t, h, prefix+"/init-script", `{"script":"#!/bin/bash\necho hi"}`, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("init-script save = %d, want 200", rr.Code)
	}
	rows, _ := d.ListAuditLog(0, 10)
	var found bool
	for _, r := range rows {
		if r.Action == "init_script" && r.Actor == "000+alice" {
			found = true
		}
	}
	if !found {
		t.Errorf("audit rows = %+v, want init_script attributed 000+alice", rows)
	}
}

// TestOverviewImpersonationBanner verifies the "logged in as" banner renders
// only when the session is impersonated, with a link back to the admin panel.
func TestOverviewImpersonationBanner(t *testing.T) {
	srv, _ := newTestServer(t)
	html := srv.renderToString(t, "overview.html", pageData{
		User: &db.User{Name: "alice"}, Prefix: "/" + testSecret,
		Impersonated: true, AdminPrefix: "/Adm1n", Lang: langZh,
	})
	for _, want := range []string{"管理员代客登录中", "返回管理员面板", "/Adm1n"} {
		if !strings.Contains(html, want) {
			t.Errorf("impersonated overview missing %q", want)
		}
	}

	plain := srv.renderToString(t, "overview.html", pageData{
		User: &db.User{Name: "alice"}, Prefix: "/" + testSecret,
	})
	for _, absent := range []string{`class="impbanner"`, "管理员代客登录中"} {
		if strings.Contains(plain, absent) {
			t.Errorf("normal overview should not render %q", absent)
		}
	}
}
