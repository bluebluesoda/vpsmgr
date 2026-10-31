package panel

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"vpsmgr/internal/db"
	"vpsmgr/internal/mgr"
	"vpsmgr/internal/pw"
)

func TestSnapshotScheduleControls(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		limit, fixed, auto    int
		enabled, listError    bool
		create, schedule, pin bool
	}{
		{"off free", 3, 1, 0, false, false, true, true, true},
		{"off full", 3, 3, 0, false, false, false, false, false},
		{"on reserve", 3, 2, 0, true, false, false, true, false},
		{"on free", 3, 1, 1, true, false, true, true, true},
		{"on full can promote", 3, 1, 2, true, false, false, true, true},
		{"on full fixed cap", 3, 2, 1, true, false, false, true, false},
		{"single automatic", 1, 0, 1, true, false, false, true, false},
		{"disabled globally", 0, 0, 0, false, false, false, false, false},
		{"catalog failure", 3, 0, 0, true, true, false, false, false},
		{"over quota", 3, 1, 3, true, false, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := pageData{User: &db.User{Status: db.StatusReady}, SnapshotLimit: tc.limit,
				SnapshotSchedule: &db.SnapshotSchedule{Enabled: tc.enabled, IntervalHours: 7}}
			for i := 0; i < tc.fixed; i++ {
				d.Snapshots = append(d.Snapshots, snapshotRow{})
			}
			for i := 0; i < tc.auto; i++ {
				d.Snapshots = append(d.Snapshots, snapshotRow{Automatic: true})
			}
			if tc.listError {
				d.SnapshotListError = "catalog unavailable"
			}
			d.prepareSnapshotUI()
			if d.SnapshotCanCreate != tc.create || d.SnapshotCanSchedule != tc.schedule || d.SnapshotCanPromote != tc.pin {
				t.Fatalf("controls = create %v schedule %v promote %v; want %v %v %v", d.SnapshotCanCreate, d.SnapshotCanSchedule, d.SnapshotCanPromote, tc.create, tc.schedule, tc.pin)
			}
			if d.SnapshotFixedCount != tc.fixed || d.SnapshotAutomaticCount != tc.auto || d.SnapshotIntervalHours != 7 || d.SnapshotMaxHours != mgr.MaxSnapshotIntervalHours {
				t.Fatal("incorrect counts or interval bounds")
			}
			for _, mode := range []string{"expired", "not ready", "schedule error"} {
				copy := d
				copy.User = &db.User{Status: db.StatusReady}
				switch mode {
				case "expired":
					copy.Expired = true
				case "not ready":
					copy.User.Status = "creating"
				case "schedule error":
					copy.SnapshotScheduleError = "unavailable"
				}
				copy.prepareSnapshotUI()
				if copy.SnapshotCanCreate || copy.SnapshotCanSchedule || copy.SnapshotCanPromote {
					t.Errorf("%s permits a catalog-dependent action", mode)
				}
			}
		})
	}
}

func TestSnapshotScheduleValidationAndDisable(t *testing.T) {
	srv, d := newTestServer(t)
	hash, err := pw.Hash("pw")
	if err != nil {
		t.Fatal(err)
	}
	alice, err := d.CreateUser("alice", hash, "192.0.2.2", 1, 30001, 10000, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	bob, err := d.CreateUser("bob", hash, "192.0.2.3", 2, 30002, 10100, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{alice.ID, bob.ID} {
		if err := d.SaveSnapshotSchedule(id, true, 7, 2000000000); err != nil {
			t.Fatal(err)
		}
	}
	h := srv.Handler()
	prefix := srv.prefix()
	cookie := loginAndCookie(t, h, prefix, "alice", "pw")
	body := doReq(t, h, http.MethodGet, prefix, nil, cookie).Body.String()
	if !strings.Contains(body, "Checkpoint list unavailable") || strings.Contains(body, "No checkpoints yet") {
		t.Error("overview did not preserve the real Incus catalog error")
	}
	if !strings.Contains(snapshotTag(t, body, `id="snapCreate"`), "disabled") || strings.Contains(snapshotTag(t, body, `id="snapScheduleDisable"`), " disabled") {
		t.Error("overview error controls do not permit only catalog-independent disable")
	}
	for _, raw := range []string{"", "0", "-1", "+1", "1.5", "1e2", " 1", "1 ", "１", "9223372036854775808", strconv.Itoa(mgr.MaxSnapshotIntervalHours + 1)} {
		rr := doReq(t, h, http.MethodPost, prefix+"/snapshot-schedule", url.Values{"enabled": {"1"}, "hours": {raw}}, cookie)
		if rr.Code != http.StatusFound {
			t.Fatalf("hours %q status = %d", raw, rr.Code)
		}
		flash := doReq(t, h, http.MethodPost, prefix+"/flash", nil, cookie).Body.String()
		if !strings.Contains(flash, "hours must be an integer") {
			t.Errorf("hours %q bypassed integer validation: %s", raw, flash)
		}
	}
	for _, enabled := range []string{"", "true", "2", "-1"} {
		doReq(t, h, http.MethodPost, prefix+"/snapshot-schedule", url.Values{"enabled": {enabled}, "hours": {"1"}}, cookie)
		if flash := doReq(t, h, http.MethodPost, prefix+"/flash", nil, cookie).Body.String(); !strings.Contains(flash, "enabled must be 0 or 1") {
			t.Errorf("invalid enabled %q: %s", enabled, flash)
		}
	}
	// Valid boundaries reach the real manager and fail on the dead Incus socket.
	for _, raw := range []string{"1", strconv.Itoa(mgr.MaxSnapshotIntervalHours)} {
		doReq(t, h, http.MethodPost, prefix+"/snapshot-schedule", url.Values{"enabled": {"1"}, "hours": {raw}}, cookie)
		flash := doReq(t, h, http.MethodPost, prefix+"/flash", nil, cookie).Body.String()
		if strings.Contains(flash, "hours must be an integer") || !strings.Contains(flash, "error: ") {
			t.Errorf("valid boundary %s did not reach manager: %s", raw, flash)
		}
	}
	doReq(t, h, http.MethodPost, prefix+"/snapshot-promote", url.Values{"name": {"../bad"}}, cookie)
	rows, err := d.ListAuditLog(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if strings.HasPrefix(row.Action, "snapshot.") {
			t.Errorf("failed operation was audited: %+v", row)
		}
	}
	// Disable succeeds without Incus, ignores invalid hours and untrusted IDs.
	rr := doReq(t, h, http.MethodPost, prefix+"/snapshot-schedule", url.Values{
		"enabled": {"0"}, "hours": {"not an integer"}, "userID": {fmt.Sprint(bob.ID)}, "user": {"bob"},
	}, cookie)
	if rr.Code != http.StatusFound {
		t.Fatalf("disable status = %d", rr.Code)
	}
	for _, u := range []*db.User{alice, bob} {
		schedule, err := srv.mgr.SnapshotSchedule(u.Name)
		if err != nil || schedule == nil {
			t.Fatalf("schedule %s: %+v %v", u.Name, schedule, err)
		}
		if schedule.Enabled != (u.Name == "bob") || schedule.IntervalHours != 7 {
			t.Errorf("disable changed wrong user or interval: %+v", schedule)
		}
	}
	rows, err = d.ListAuditLog(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Action == "snapshot.schedule.disable" && row.Actor == "alice" {
			found = true
		}
	}
	if !found {
		t.Fatal("successful disable missing audit event")
	}
	sess, err := d.CreateImpersonatedSession(alice.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	impersonated := &http.Cookie{Name: "vpsmgr_session", Value: sess.Token, Path: prefix}
	doReq(t, h, http.MethodPost, prefix+"/snapshot-schedule", url.Values{"enabled": {"0"}}, impersonated)
	rows, err = d.ListAuditLog(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, row := range rows {
		if row.Action == "snapshot.schedule.disable" && row.Actor == "000+alice" {
			found = true
		}
	}
	if !found {
		t.Fatal("impersonated disable missing attributed audit event")
	}
}

func TestSnapshotScheduleRouteGuards(t *testing.T) {
	srv, d := newTestServer(t)
	hash, _ := pw.Hash("pw")
	_, err := d.CreateUser("alice", hash, "192.0.2.2", 1, 30001, 10000, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	_, err = d.CreateUserFull("expired", hash, "192.0.2.3", 2, 30002, 10100, 1, 1024, 10, 0,
		db.StatusReady, "", 0, "", "2000-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	h := srv.Handler()
	cookie := loginAndCookie(t, h, srv.prefix(), "alice", "pw")
	expired := loginAndCookie(t, h, srv.prefix(), "expired", "pw")
	for _, path := range []string{"/snapshot-schedule", "/snapshot-promote"} {
		target := srv.p(path)
		if rr := doReq(t, h, http.MethodPost, target, url.Values{}, nil); rr.Header().Get("Location") != srv.p("/login") {
			t.Errorf("%s did not require auth", path)
		}
		if rr := doReq(t, h, http.MethodGet, target, nil, cookie); rr.Code != http.StatusNotFound {
			t.Errorf("GET %s status = %d", path, rr.Code)
		}
		r := httptest.NewRequest(http.MethodPost, target, strings.NewReader("enabled=0"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "https://attacker.example")
		r.AddCookie(cookie)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Errorf("cross-origin %s status = %d", path, w.Code)
		}
		doReq(t, h, http.MethodPost, target, url.Values{"enabled": {"0"}, "name": {"snap-x"}}, expired)
		if flash := doReq(t, h, http.MethodPost, srv.p("/flash"), nil, expired).Body.String(); !strings.Contains(strings.ToLower(flash), "expired") {
			t.Errorf("expired %s was not blocked: %s", path, flash)
		}
	}
}

func snapshotTag(t *testing.T, body, marker string) string {
	t.Helper()
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("missing element %s", marker)
	}
	start := strings.LastIndex(body[:i], "<")
	end := strings.Index(body[i:], ">")
	return body[start : i+end+1]
}

func TestSnapshotScheduleTemplate(t *testing.T) {
	srv, _ := newTestServer(t)
	for _, lang := range []string{"en", "zh"} {
		d := pageData{User: &db.User{Name: "alice", Status: db.StatusReady}, Lang: lang,
			Prefix: srv.prefix(), SnapshotLimit: 3, ShareEnabled: true,
			SnapshotSchedule: &db.SnapshotSchedule{Enabled: true, IntervalHours: 7,
				NextRunAt: 2000000000, LastRunAt: 1900000000, LastSuccessAt: 1800000000, LastError: "<unsafe>"},
			Snapshots: []snapshotRow{{Name: "snap-fixed", CreatedAt: "2026-01-01T00:00:00Z"},
				{Name: "snap-auto", Automatic: true, CreatedAt: "2026-01-02T00:00:00Z"}}}
		d.prepareSnapshotUI()
		body := srv.renderToString(t, "overview.html", d)
		for _, want := range []string{`name="hours" min="1" max="` + strconv.Itoa(mgr.MaxSnapshotIntervalHours) + `" step="1"`,
			`value="7"`, `onclick="promoteSnap('snap-auto')"`, `onclick="shareSnap('snap-fixed'`,
			`onclick="restoreSnap('snap-auto')"`, `onclick="delSnap('snap-auto')"`,
			`class="schedule-ts" data-ts="2033-05-18T03:33:20Z"`, `&lt;unsafe&gt;`,
			`All automatic checkpoints will become fixed. No checkpoints will be deleted.`,
			`所有自动检查点都会转为固定检查点，不会删除任何检查点`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s template missing %q", lang, want)
			}
		}
		if strings.Contains(body, `onclick="shareSnap('snap-auto'`) {
			t.Error("automatic checkpoint offers sharing")
		}
		d.SnapshotListError = "catalog failed"
		d.Snapshots = nil
		d.prepareSnapshotUI()
		body = srv.renderToString(t, "overview.html", d)
		for _, id := range []string{"snapCreate", "snapScheduleSave"} {
			if !strings.Contains(snapshotTag(t, body, `id="`+id+`"`), "disabled") {
				t.Errorf("catalog error did not disable %s", id)
			}
		}
		if strings.Contains(snapshotTag(t, body, `id="snapScheduleDisable"`), " disabled") {
			t.Error("catalog error blocked disabling the schedule")
		}
		if strings.Contains(body, "No checkpoints yet") || strings.Contains(body, "暂无检查点") {
			t.Error("catalog failure was presented as an empty list")
		}
	}
}
