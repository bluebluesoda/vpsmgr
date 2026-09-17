package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"vpsmgr/internal/db"
)

// The WebSocket handshake is a GET, so the panel's POST CSRF check never sees
// it and SameSite does not apply the way it does to a form post. These are the
// assertions that matter: without them any page could open a shell on the
// user's container with the user's cookie attached.
func TestTerminalRejectsForeignOrigin(t *testing.T) {
	srv, d := newTestServer(t)
	cookie := sessionCookie(t, d, "alice")
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	url := "ws" + strings.TrimPrefix(ts.URL, "http") + srv.p("/terminal")
	cases := []struct {
		name   string
		origin string
	}{
		{"foreign origin", "http://evil.test"},
		{"sibling subdomain", "http://sub.example.test"},
		{"no origin at all", ""},
	}
	for _, tc := range cases {
		h := http.Header{}
		h.Set("Cookie", cookie.String())
		if tc.origin != "" {
			h.Set("Origin", tc.origin)
		}
		conn, resp, err := websocket.DefaultDialer.Dial(url, h)
		if err == nil {
			conn.Close()
			t.Errorf("%s: handshake succeeded, want it refused", tc.name)
			continue
		}
		if resp == nil || resp.StatusCode != http.StatusForbidden {
			code := 0
			if resp != nil {
				code = resp.StatusCode
			}
			t.Errorf("%s: status = %d, want 403", tc.name, code)
		}
	}
}

// sessionCookie creates a user and a live panel session for it.
func sessionCookie(t *testing.T, d *db.DB, name string) *http.Cookie {
	t.Helper()
	u, err := d.CreateUser(name, "hash", "10.115.0.9", 9, 30009, 10900, 10, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := d.CreateSession(u.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Cookie{Name: "vpsmgr_session", Value: sess.Token}
}

// A same-origin handshake still needs a session; without one the request goes
// through the ordinary auth redirect, which is not a websocket upgrade.
func TestTerminalRequiresSession(t *testing.T) {
	srv, _ := newTestServer(t)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	url := "ws" + strings.TrimPrefix(ts.URL, "http") + srv.p("/terminal")
	h := http.Header{}
	h.Set("Origin", ts.URL)
	conn, resp, err := websocket.DefaultDialer.Dial(url, h)
	if err == nil {
		conn.Close()
		t.Fatal("handshake succeeded without a session")
	}
	if resp == nil || resp.StatusCode == http.StatusSwitchingProtocols {
		t.Fatalf("expected no upgrade, got %+v", resp)
	}
}

// The engine is embedded and served from the panel prefix, because the panel's
// CSP allows no third-party scripts.
func TestTerminalAssetServed(t *testing.T) {
	srv, _ := newTestServer(t)
	h := srv.Handler()
	rr := doReq(t, h, http.MethodGet, srv.p(termAssetPath), nil, nil)
	if rr.Code != http.StatusFound { // no session yet: redirected to login
		t.Fatalf("asset without a session = %d, want the login redirect", rr.Code)
	}
}

func TestTerminalSizeFromQuery(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/terminal?cols=132&rows=43", nil)
	got := termSizeFrom(r)
	if got.Cols != 132 || got.Rows != 43 {
		t.Errorf("size = %+v, want 132x43", got)
	}
	r = httptest.NewRequest(http.MethodGet, "/terminal", nil)
	got = termSizeFrom(r)
	if got.Cols != 0 || got.Rows != 0 {
		t.Errorf("size = %+v, want zeroes for the lx layer to normalise", got)
	}
}

func TestTermRegistryLimits(t *testing.T) {
	reg := newTermRegistry()
	for i := 0; i < termMaxPerUser; i++ {
		if !reg.acquire("alice") {
			t.Fatalf("acquire %d refused below the cap", i+1)
		}
	}
	if reg.acquire("alice") {
		t.Error("acquire above the cap succeeded")
	}
	if !reg.acquire("bob") {
		t.Error("one user's sessions must not block another's")
	}
	for i := 0; i < termMaxPerUser; i++ {
		reg.release("alice")
	}
	if !reg.acquire("alice") {
		t.Error("released slots were not reclaimed")
	}
	// Release must not go negative.
	reg.release("alice")
	reg.release("alice")
	reg.release("alice")
	if !reg.acquire("alice") {
		t.Error("registry went negative")
	}
}

// The shell lives in its own window, so the panel serves a page for it rather
// than opening a modal.
func TestWebSSHPage(t *testing.T) {
	srv, d := newTestServer(t)
	cookie := sessionCookie(t, d, "alice")
	h := srv.Handler()

	rr := doReq(t, h, http.MethodGet, srv.p("/webssh"), nil, cookie)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /webssh = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		`id="termMount"`, `id="fsBtn"`, `id="reBtn"`,
		srv.p(termAssetPath), `'/terminal'`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("webssh page missing %q", want)
		}
	}
	// It must never carry the panel's own chrome (it is a bare terminal window).
	if strings.Contains(body, `id="kbModal"`) || strings.Contains(body, "Sticky notes") {
		t.Error("webssh page should be standalone, not the overview")
	}

	// Without a session it is the ordinary login redirect.
	rr = doReq(t, h, http.MethodGet, srv.p("/webssh"), nil, nil)
	if rr.Code != http.StatusFound {
		t.Errorf("GET /webssh without a session = %d, want the login redirect", rr.Code)
	}
}

// The button belongs next to Start/Stop/Restart, and must read as a shell.
func TestOverviewWebSSHButton(t *testing.T) {
	srv, _ := newTestServer(t)
	body := srv.renderToString(t, "overview.html", pageData{
		User: &db.User{Name: "alice"}, Prefix: "/" + testSecret, Lang: langEn,
	})
	i := strings.Index(body, `id="sshBtn"`)
	if i < 0 {
		t.Fatal("overview is missing the Web SSH button")
	}
	if !strings.Contains(body[:i], `value="restart"`) {
		t.Error("the Web SSH button should sit right after Start/Stop/Restart")
	}
	if !strings.Contains(body[i-200:i+200], `class="btn ssh"`) {
		t.Error("the Web SSH button should carry the ssh style")
	}
	if strings.Contains(body, `id="termModal"`) {
		t.Error("the terminal must no longer be a modal")
	}
}
