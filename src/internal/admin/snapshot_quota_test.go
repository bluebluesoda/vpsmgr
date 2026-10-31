package admin

import (
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestParseSnapshotLimit(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		creation  bool
		want      int
		defaulted bool
		invalid   bool
	}{
		{"creation default", "", true, 0, true, false},
		{"creation whitespace default", "  ", true, 0, true, false},
		{"creation disabled", "0", true, 0, false, false},
		{"creation limit", " 7 ", true, 7, false, false},
		{"edit disabled", "0", false, 0, false, false},
		{"edit limit", "3", false, 3, false, false},
		{"edit blank", "", false, 0, false, true},
		{"negative", "-1", true, 0, false, true},
		{"malformed", "bad", true, 0, false, true},
		{"fraction", "1.5", true, 0, false, true},
		{"overflow", "999999999999999999999999", true, 0, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSnapshotLimit(tc.raw, tc.creation)
			if (err != nil) != tc.invalid {
				t.Fatalf("limit=%v error=%v, invalid=%v", got, err, tc.invalid)
			}
			if tc.invalid {
				return
			}
			if (got == nil) != tc.defaulted {
				t.Fatalf("limit=%v, defaulted=%v", got, tc.defaulted)
			}
			if got != nil && *got != tc.want {
				t.Fatalf("limit=%d, want %d", *got, tc.want)
			}
		})
	}
}

func TestSnapshotQuotaFormValidation(t *testing.T) {
	srv, d := newTestServer(t)
	u, err := d.CreateUser("alice", "h", "192.0.2.2", 1, 30001, 10000, 10, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateSnapshotLimit(u.ID, 7); err != nil {
		t.Fatal(err)
	}
	if err := srv.mgr.SetSnapshotShareEnabled(true); err != nil {
		t.Fatal(err)
	}
	setAdminPass(t, srv, "correct-horse-battery")
	h := srv.Handler()
	prefix := "/" + testAdminSecret
	cookie := adminLogin(t, h, prefix, "correct-horse-battery")
	for _, route := range []string{"user-add", "user-batch", "user-quota"} {
		for _, raw := range []string{"-1", "bad", "1.5", "999999999999999999999999"} {
			t.Run(route+"/"+raw, func(t *testing.T) {
				form := url.Values{
					"name": {"bob"}, "names": {"bob"}, "share": {"invalid-code"},
					"cpu": {"1"}, "mem": {"1024"}, "disk": {"10"},
					"bandwidth": {"0"}, "days": {"0"}, "snapshot_limit": {raw},
				}
				if route == "user-quota" {
					form.Set("name", "alice")
				}
				rr := doReq(t, h, http.MethodPost, prefix+"/"+route, form, cookie)
				if rr.Code != http.StatusFound {
					t.Fatalf("status=%d", rr.Code)
				}
				msg, _, _, _ := srv.flash.Pop(cookie.Value)
				if !strings.Contains(msg, "snapshot limit must be a nonnegative integer") {
					t.Fatalf("flash=%q", msg)
				}
			})
		}
	}
	// An old form without the new field must pass field validation; a blank
	// field in a new edit form must not silently reset the quota to default.
	for _, present := range []bool{false, true} {
		form := url.Values{"name": {"alice"}, "cpu": {"1"}, "mem": {"1024"}, "disk": {"10"}, "bandwidth": {"0"}}
		if present {
			form.Set("snapshot_limit", "")
		}
		doReq(t, h, http.MethodPost, prefix+"/user-quota", form, cookie)
		msg, _, _, _ := srv.flash.Pop(cookie.Value)
		if strings.Contains(msg, "snapshot limit must be") != present {
			t.Fatalf("present=%v flash=%q", present, msg)
		}
	}
	fresh, err := d.GetUserByName("alice")
	if err != nil || fresh.SnapshotLimit != 7 || fresh.CPU != u.CPU || fresh.BandwidthQuotaGB != u.BandwidthQuotaGB {
		t.Fatalf("rejected/legacy form changed quotas: user=%+v error=%v", fresh, err)
	}
	if users, err := d.ListUsers(); err != nil || len(users) != 1 {
		t.Fatalf("rejected forms created users: count=%d error=%v", len(users), err)
	}
}

func TestOverviewSnapshotQuotaUI(t *testing.T) {
	srv, d := newTestServer(t)
	srv.cfg.Snapshots.Limit = 9
	// The manager has the same config pointer as the server.
	if got := srv.buildPageData("", "").DefaultSnapshotLimit; got != 9 {
		t.Fatalf("default=%d, want 9", got)
	}
	u, err := d.CreateUser("alice", "h", "192.0.2.2", 1, 30001, 10000, 10, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateSnapshotLimit(u.ID, 4); err != nil {
		t.Fatal(err)
	}
	data := pageData{Lang: langEn, DefaultSnapshotLimit: 9, ShareEnabled: true}
	srv.loadUsers(&data)
	if len(data.Users) != 1 || data.Users[0].SnapshotLimit != 4 {
		t.Fatalf("per-user view=%+v", data.Users)
	}
	fields := regexp.MustCompile(`(?s)<div class="field[^"]*"><label>(?:快照配额|Snapshot limit)</label>(.*?)</div>`)
	for _, lang := range []string{langEn, langZh} {
		for _, limit := range []int{0, 4} {
			t.Run(lang+"/"+strconv.Itoa(limit), func(t *testing.T) {
				data.Lang = lang
				data.Users[0].SnapshotLimit = limit
				html := srv.renderToString(t, "admin_overview.html", data)
				label, disabled := "snapshot limit: ", "no new snapshots"
				if lang == langZh {
					label, disabled = "快照配额: ", "禁止新建"
				}
				for _, want := range []string{
					`id="cfSnapshots"`, `id="quotaSnapshots"`,
					label + strconv.Itoa(limit),
					`document.getElementById('quotaSnapshots').value = parseInt(snapshots)`,
				} {
					if !strings.Contains(html, want) {
						t.Errorf("missing %q", want)
					}
				}
				for _, removed := range []string{
					"留空使用默认值", "Blank = default", "0 = 禁止新建快照", "0 = no new snapshots",
					"不能低于已有快照和自动快照预留数量", "cannot be below existing snapshots and automatic reservation",
				} {
					if strings.Contains(html, removed) {
						t.Errorf("obsolete snapshot helper remains: %q", removed)
					}
				}
				matches := fields.FindAllStringSubmatch(html, -1)
				if len(matches) != 3 {
					t.Fatalf("snapshot fields=%d, want create/edit/batch", len(matches))
				}
				for _, field := range matches {
					if strings.Contains(field[1], "<small") || strings.Count(field[1], "<input") != 1 {
						t.Errorf("snapshot field should contain only its numeric input: %s", field[0])
					}
					for _, want := range []string{`name="snapshot_limit"`, `type="number"`, `min="0"`, `step="1"`} {
						if !strings.Contains(field[1], want) {
							t.Errorf("snapshot input lost %q: %s", want, field[1])
						}
					}
					if strings.Contains(field[1], `id="quotaSnapshots"`) {
						if !strings.Contains(field[1], " required") {
							t.Error("quota edit input must remain required")
						}
					} else if !strings.Contains(field[1], `placeholder="9"`) || strings.Contains(field[1], " required") {
						t.Error("create/batch inputs must remain optional with the configured default placeholder")
					}
				}
				if limit == 0 && !strings.Contains(html, label+"0 ("+disabled+")") {
					t.Error("zero quota does not clearly disable new snapshots")
				}
			})
		}
	}
}
