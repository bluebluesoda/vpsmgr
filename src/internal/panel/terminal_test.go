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
