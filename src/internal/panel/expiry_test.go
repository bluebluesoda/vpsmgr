package panel

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"vpsmgr/internal/db"
	"vpsmgr/internal/pw"
)

// TestExpiredAccountLocked: an expired account is read-only in the user panel —
// mutations are refused with a clear reason, and the overview shows the lock
// instead of the action buttons.
func TestExpiredAccountLocked(t *testing.T) {
	srv, d := newTestServer(t)
	h := srv.Handler()
	prefix := "/" + testSecret
	hash, _ := pw.Hash("pw")
	if _, err := d.CreateUserFull("alice", hash, "10.42.0.2", 1, 30001, 10000, 1, 1024, 10, 0,
		db.StatusReady, "", 0, "2000-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	cookie := loginAndCookie(t, h, prefix, "alice", "pw")

	// A mutation is blocked before it reaches the handler.
	rr := doReq(t, h, http.MethodPost, prefix+"/power", url.Values{"action": {"start"}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("expired POST /power = %d, want 302", rr.Code)
	}
	fr := doReq(t, h, http.MethodPost, prefix+"/flash", nil, cookie)
	if !strings.Contains(strings.ToLower(fr.Body.String()), "expired") {
		t.Fatalf("blocked mutation did not report the expiry: %s", fr.Body.String())
	}

	// Changing the panel password IS still allowed on an expired account.
	rr = doReq(t, h, http.MethodPost, prefix+"/password",
		url.Values{"new_password": {"NewPass1234"}, "confirm_password": {"NewPass1234"}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("expired POST /password = %d, want 302", rr.Code)
	}
	fr = doReq(t, h, http.MethodPost, prefix+"/flash", nil, cookie)
	if strings.Contains(strings.ToLower(fr.Body.String()), "expired") {
		t.Fatalf("password change was blocked on an expired account: %s", fr.Body.String())
	}

	// The overview shows the banner + locked note and hides the action buttons.
	rr = doReq(t, h, http.MethodGet, prefix, nil, cookie)
	body := rr.Body.String()
	if !strings.Contains(body, "expbanner") {
		t.Error("expired overview missing the banner")
	}
	if !strings.Contains(body, "expiry-remaining") {
		t.Error("expired overview missing the validity countdown")
	}
	for _, absent := range []string{`id="reBtn"`, `id="snapBtn"`, `id="kbBtn"`} {
		if strings.Contains(body, absent) {
			t.Errorf("expired overview still shows the action button %s", absent)
		}
	}
}
