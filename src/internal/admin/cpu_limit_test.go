package admin

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"vpsmgr/internal/mgr"
)

// The CPU limit rule is edited in the admin panel (not via the CLI/config) and
// stored in the DB, so a save has to apply on the spot and an out-of-range value
// must leave the stored rule alone.
func TestCPULimitCardSave(t *testing.T) {
	srv, d := newTestServer(t)
	setAdminPass(t, srv, "correct-horse-battery")
	h := srv.Handler()
	prefix := "/" + testAdminSecret
	cookie := adminLogin(t, h, prefix, "correct-horse-battery")

	// The overview carries the editor, prefilled with the default rule.
	rr := doReq(t, h, http.MethodGet, prefix, nil, cookie)
	if body := rr.Body.String(); !strings.Contains(body, `action="`+prefix+`/cpu-limit"`) ||
		!strings.Contains(body, `name="window_minutes"`) {
		t.Fatalf("overview missing the CPU limit editor (code %d)", rr.Code)
	}
	if def := srv.mgr.CPULimitRule(); def != mgr.DefaultCPULimitRule() {
		t.Fatalf("fresh rule = %+v, want the disabled default", def)
	}

	// Save: on, 3 minutes over 25% -> 0.2 core for 1h05m.
	rr = doReq(t, h, http.MethodPost, prefix+"/cpu-limit", url.Values{
		"enabled": {"on"}, "window_minutes": {"3"}, "percent": {"25"},
		"cores": {"0.2"}, "duration_hours": {"1"}, "duration_minutes": {"5"},
	}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("cpu-limit save = %d, want 302", rr.Code)
	}
	want := mgr.CPULimitRule{Enabled: true, WindowMinutes: 3, Percent: 25, CoresX10: 2, DurationSeconds: 3900}
	if got := srv.mgr.CPULimitRule(); got != want {
		t.Fatalf("stored rule = %+v, want %+v", got, want)
	}
	// It survives a restart (a new manager over the same DB).
	if got := mgr.New(srv.cfg, d).CPULimitRule(); got != want {
		t.Fatalf("rule did not survive a restart: %+v", got)
	}
	// The save is audited.
	rows, err := d.ListAuditLog(0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var audited bool
	for _, r := range rows {
		if r.Actor == "000" && r.Action == "cpu_limit.update" {
			audited = true
		}
	}
	if !audited {
		t.Errorf("no cpu_limit.update audit row: %+v", rows)
	}

	// Out-of-range input is refused and changes nothing.
	for _, bad := range []url.Values{
		{"enabled": {"on"}, "window_minutes": {"0"}, "percent": {"60"}, "cores": {"0.5"}, "duration_hours": {"1"}, "duration_minutes": {"0"}},
		{"enabled": {"on"}, "window_minutes": {"10"}, "percent": {"101"}, "cores": {"0.5"}, "duration_hours": {"1"}, "duration_minutes": {"0"}},
		{"enabled": {"on"}, "window_minutes": {"10"}, "percent": {"60"}, "cores": {"1.5"}, "duration_hours": {"1"}, "duration_minutes": {"0"}},
		{"enabled": {"on"}, "window_minutes": {"10"}, "percent": {"60"}, "cores": {"0.5"}, "duration_hours": {"0"}, "duration_minutes": {"0"}},
		{"enabled": {"on"}, "window_minutes": {"10"}, "percent": {"60"}, "cores": {"0.5"}, "duration_hours": {"x"}, "duration_minutes": {"0"}},
	} {
		doReq(t, h, http.MethodPost, prefix+"/cpu-limit", bad, cookie)
		if got := srv.mgr.CPULimitRule(); got != want {
			t.Fatalf("invalid save (%v) changed the rule to %+v", bad, got)
		}
	}

	// Saving while disabled keeps the parameters and turns the limit off (the
	// enforcement loop then restores anything it had capped).
	rr = doReq(t, h, http.MethodPost, prefix+"/cpu-limit", url.Values{
		"window_minutes": {"3"}, "percent": {"25"},
		"cores": {"0.2"}, "duration_hours": {"1"}, "duration_minutes": {"5"},
	}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("disable save = %d, want 302", rr.Code)
	}
	off := want
	off.Enabled = false
	if got := srv.mgr.CPULimitRule(); got != off {
		t.Fatalf("disabled rule = %+v, want %+v", got, off)
	}
}
