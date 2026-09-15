package admin

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"vpsmgr/internal/db"
	"vpsmgr/internal/mgr"
)

// TestAdminExpiredUserLocked: an expired account is read-only for the admin —
// a quota change is refused, while extending the deadline (and deleting) still
// works. This covers requireTargetActive and handleUserExpiry.
func TestAdminExpiredUserLocked(t *testing.T) {
	srv, d := newTestServer(t)
	setAdminPass(t, srv, "correct-horse-battery")
	h := srv.Handler()
	prefix := "/" + testAdminSecret
	cookie := adminLogin(t, h, prefix, "correct-horse-battery")

	u, err := d.CreateUserFull("alice", "h", "10.42.0.2", 1, 30001, 10000, 1, 1024, 10, 0,
		db.StatusReady, "", "2000-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}

	// A quota change on the expired account is refused and does not apply.
	rr := doReq(t, h, http.MethodPost, prefix+"/user-quota",
		url.Values{"name": {"alice"}, "cpu": {"2"}, "mem": {"1024"}, "disk": {"10"}, "bandwidth": {"0"}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("expired /user-quota = %d, want 302", rr.Code)
	}
	if got, _ := d.GetUserByName("alice"); got.CPU != u.CPU {
		t.Fatalf("quota changed on an expired account: cpu=%d", got.CPU)
	}

	// Resetting the panel password IS allowed on an expired account.
	rr = doReq(t, h, http.MethodPost, prefix+"/reset-panel-pass", url.Values{"name": {"alice"}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("expired /reset-panel-pass = %d, want 302", rr.Code)
	}
	if got, _ := d.GetUserByName("alice"); got.PassHash == "h" {
		t.Fatal("admin password reset was blocked on an expired account")
	}

	// Deleting is still allowed (handled by the delete route, not locked).
	// Extending the deadline is the other allowed operation.
	rr = doReq(t, h, http.MethodPost, prefix+"/user-expiry",
		url.Values{"name": {"alice"}, "extend": {"30d"}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("extend = %d, want 302", rr.Code)
	}
	got, err := d.GetUserByName("alice")
	if err != nil {
		t.Fatal(err)
	}
	if mgr.IsExpired(got.ExpiresAt, time.Now().UTC()) {
		t.Fatalf("extend did not push the deadline into the future: %q", got.ExpiresAt)
	}

	// "forever" clears the deadline.
	rr = doReq(t, h, http.MethodPost, prefix+"/user-expiry",
		url.Values{"name": {"alice"}, "extend": {"forever"}}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("extend forever = %d, want 302", rr.Code)
	}
	if got, _ := d.GetUserByName("alice"); got.ExpiresAt != "" {
		t.Fatalf("forever did not clear the deadline: %q", got.ExpiresAt)
	}
}
