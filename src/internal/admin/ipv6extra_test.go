package admin

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
)

// TestExtraPrefixUI renders the pages that carry a whole-/64 control with
// net.ipv6_extra_prefix configured. Those template branches are dead on a host
// without an extra prefix — which is every default install and every test that
// does not set one — so a mistake there would only surface once an operator
// turned the feature on.
func TestExtraPrefixUI(t *testing.T) {
	// newHost returns an admin server in prefix mode with extra configured, one
	// account per given block, plus the handler, prefix and free-block count.
	newHost := func(t *testing.T, extra string, blocks ...string) (http.Handler, string, int) {
		t.Helper()
		srv, d := newTestServer(t)
		srv.cfg.Net.IPv6Subnet = "2602:fada:6::/64"
		srv.cfg.Net.IPv6Mode = cfg.IPv6ModePrefix
		srv.cfg.Net.IPv6ExtraPrefix = extra
		setAdminPass(t, srv, "correct-horse-battery")
		for i, b := range blocks {
			if _, err := d.CreateUserFull(fmt.Sprintf("u%d", i), "h", fmt.Sprintf("10.42.0.%d", i+2),
				i+1, 30001+i, 10000+i*100, 1, 1024, 10, 0, db.StatusReady, "", int64(i+1), b, ""); err != nil {
				t.Fatal(err)
			}
		}
		total, reserved, used, free, err := srv.mgr.ExtraCapacity()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("extra prefix %s: total=%d reserved=%d used=%d free=%d", extra, total, reserved, used, free)
		return srv.Handler(), "/" + testAdminSecret, free
	}

	t.Run("blocks available", func(t *testing.T) {
		h, prefix, free := newHost(t, "2001:db8:1234::/56", "2001:db8:1234::/64")
		ck := adminLogin(t, h, prefix, "correct-horse-battery")

		// The IPv6 page: the editor (pre-filled), the capacity line and the
		// list of assigned blocks with their owner.
		rr := doReq(t, h, http.MethodGet, prefix+"/ipv6pool", nil, ck)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /ipv6pool = %d: %s", rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		for _, want := range []string{
			"ipv6extra-set",              // the save target
			`value="2001:db8:1234::/56"`, // the configured prefix
			"2001:db8:1234::/64",         // an assigned block
			"u0",                         // its owner
			fmt.Sprintf("%d free", free), // the capacity readout
			"cannot be taken back by editing quotas",
		} {
			if !strings.Contains(body, want) {
				t.Errorf("IPv6 page missing %q", want)
			}
		}

		// The overview: the create form offers a block, ticked by default.
		rr = doReq(t, h, http.MethodGet, prefix, nil, ck)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET / = %d: %s", rr.Code, rr.Body.String())
		}
		body = rr.Body.String()
		label := fmt.Sprintf("Assign a whole /64 (%d left)", free)
		if !strings.Contains(body, label) {
			t.Errorf("create form missing %q", label)
		}
		if !strings.Contains(body, `name="extra64" value="1" style="width:auto;flex:none" checked`) {
			t.Error("the create form's /64 checkbox is not ticked by default")
		}
		if strings.Contains(body, `name="extra64" value="1" style="width:auto;flex:none" disabled`) {
			t.Error("the create form's /64 checkbox is disabled although blocks are free")
		}
	})

	t.Run("exhausted", func(t *testing.T) {
		// A /63 holds exactly two /64s; both are taken, so the pool is empty and
		// the control has to grey out instead of disappearing.
		h, prefix, free := newHost(t, "2001:db8:5678::/63", "2001:db8:5678::/64", "2001:db8:5678:1::/64")
		if free != 0 {
			t.Fatalf("free = %d, want an exhausted pool", free)
		}
		ck := adminLogin(t, h, prefix, "correct-horse-battery")
		rr := doReq(t, h, http.MethodGet, prefix, nil, ck)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET / = %d: %s", rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		if !strings.Contains(body, "Assign a whole /64 (0 left)") {
			t.Error("exhausted pool does not show a 0-left label")
		}
		if !strings.Contains(body, `name="extra64" value="1" style="width:auto;flex:none" disabled`) {
			t.Error("exhausted pool does not disable the /64 checkbox")
		}
	})

	t.Run("none mode with extra prefix", func(t *testing.T) {
		srv, _ := newTestServer(t)
		srv.cfg.Net.IPv6Subnet = ""
		srv.cfg.Net.IPv6Mode = cfg.IPv6ModeNone
		srv.cfg.Net.IPv6ExtraPrefix = "2a12:5e41:25de:8800::/56"
		setAdminPass(t, srv, "correct-horse-battery")
		h := srv.Handler()
		prefix := "/" + testAdminSecret
		ck := adminLogin(t, h, prefix, "correct-horse-battery")

		// Overview page renders the extra64 checkbox
		rr := doReq(t, h, http.MethodGet, prefix, nil, ck)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET / = %d: %s", rr.Code, rr.Body.String())
		}
		body := rr.Body.String()
		if !strings.Contains(body, `name="extra64"`) {
			t.Error("none mode with extra prefix does not render extra64 checkbox in overview")
		}
		if !strings.Contains(body, prefix+"/ipv6pool") {
			t.Error("none mode with extra prefix does not render IPv6 nav link")
		}

		// IPv6 page renders the extra prefix card
		rr = doReq(t, h, http.MethodGet, prefix+"/ipv6pool", nil, ck)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET /ipv6pool = %d: %s", rr.Code, rr.Body.String())
		}
		body = rr.Body.String()
		if !strings.Contains(body, `value="2a12:5e41:25de:8800::/56"`) {
			t.Error("none mode /ipv6pool page does not render the extra prefix input")
		}
	})
}

