package mgr

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
	"vpsmgr/internal/lx"
)

func snapshotTimeByName(snaps []lx.SnapshotInfo, name string) time.Time {
	for _, s := range snaps {
		if s.Name == name {
			return snapshotCreateTime(s)
		}
	}
	return time.Time{}
}

// TestSnapshotIntegrationIncus is explicitly opt-in: it creates one disposable
// account on an installed test host, exercises real disk restore/rotation and
// removes that account. Stop the old panel before testing a new schema/binary.
// CGO_ENABLED=0 VPSMGR_SNAPSHOT_INTEGRATION=1 go test -p 1 ./internal/mgr -run '^TestSnapshotIntegrationIncus$' -v
func TestSnapshotIntegrationIncus(t *testing.T) {
	if os.Getenv("VPSMGR_SNAPSHOT_INTEGRATION") != "1" {
		t.Skip("requires an installed disposable Incus host and explicit opt-in")
	}
	c, err := cfg.Load()
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.OpenWithSnapshotLimit(c.Panel.DB, c.Snapshots.Limit)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	m := New(c, d)
	shareEnabled := m.SnapshotShareEnabled()
	if err := m.SetSnapshotShareEnabled(true); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := m.SetSnapshotShareEnabled(shareEnabled); err != nil {
			t.Errorf("restore sharing toggle: %v", err)
		}
	}()
	name := fmt.Sprintf("snapcheck%xt", time.Now().UnixNano())
	limit := 5
	result, err := m.Add(name, AddOptions{CPU: 10, MemMB: 256, DiskGB: 2, SnapshotLimit: &limit})
	if err != nil {
		t.Fatalf("create disposable account: %v", err)
	}
	u := result.User
	defer func() {
		if err := m.Del(name); err != nil {
			t.Errorf("remove disposable account: %v", err)
		}
	}()
	t.Log("created disposable account; no credentials are logged")
	if err := m.SetSnapshotSchedule(name, true, 1); err != nil {
		t.Fatal(err)
	}
	snaps, err := m.SnapshotList(name)
	if err != nil || len(snaps) != 1 || !snaps[0].Automatic {
		t.Fatal("enabling did not immediately create an automatic checkpoint")
	}
	if err := m.SnapshotCreate(name); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if err := runSnapshotDue(t, m, u); err != nil {
			t.Fatal(err)
		}
	}
	snaps, err = m.SnapshotList(name)
	if err != nil || len(snaps) != 5 || fixedSnapshotCount(snaps) != 1 {
		t.Fatalf("initial quota: count=%d, fixed=%d, error=%v", len(snaps), fixedSnapshotCount(snaps), err)
	}
	if err := m.SnapshotCreate(name); err == nil {
		t.Fatal("manual creation accepted at full total quota")
	}
	if err := m.UpdateSnapshotLimit(name, 4); err == nil {
		t.Fatal("quota reduction below the real snapshot count was accepted")
	}
	if err := m.UpdateSnapshotLimit(name, 6); err != nil {
		t.Fatal(err)
	}
	if err := m.UpdateSnapshotLimit(name, 5); err != nil {
		t.Fatal(err)
	}
	// A changed new-account default must not stop an existing account's task.
	c.Snapshots.Limit = 0
	var oldestAuto string
	for _, s := range snaps {
		if s.Automatic && (oldestAuto == "" || snapshotCreateTime(s).Before(snapshotTimeByName(snaps, oldestAuto))) {
			oldestAuto = s.Name
		}
	}
	if err := runSnapshotDue(t, m, u); err != nil {
		t.Fatal(err)
	}
	snaps, err = m.SnapshotList(name)
	if err != nil || len(snaps) != 5 {
		t.Fatalf("rotation count=%d: %v", len(snaps), err)
	}
	for _, s := range snaps {
		if s.Name == oldestAuto {
			t.Fatal("oldest automatic checkpoint was not rotated")
		}
	}
	var promote string
	for _, s := range snaps {
		if s.Automatic {
			promote = s.Name
			break
		}
	}
	if _, _, err := m.ShareSnapshot(name, promote); err == nil {
		t.Fatal("automatic checkpoint was shared")
	}
	if err := m.SnapshotPromote(name, promote); err != nil {
		t.Fatal(err)
	}
	snaps, _ = m.SnapshotList(name)
	if fixedSnapshotCount(snaps) != 2 {
		t.Fatal("promotion failed at full total quota")
	}
	if err := m.SetSnapshotSchedule(name, false, 0); err != nil {
		t.Fatal(err)
	}
	snaps, _ = m.SnapshotList(name)
	if fixedSnapshotCount(snaps) != 5 {
		t.Fatal("disable did not fix all checkpoints")
	}
	if err := m.SetSnapshotSchedule(name, true, 1); err == nil {
		t.Fatal("enabled without a free position")
	}
	if err := m.SnapshotDelete(name, snaps[0].Name); err != nil {
		t.Fatal(err)
	}
	if err := m.SetSnapshotSchedule(name, true, 1); err != nil {
		t.Fatal(err)
	}
	snaps, err = m.SnapshotList(name)
	if err != nil || len(snaps) != 5 || fixedSnapshotCount(snaps) != 4 {
		t.Fatal("re-enabling did not immediately fill the free position")
	}
	if err := m.SnapshotCreate(name); err == nil {
		t.Fatal("manual create consumed the reserved automatic position")
	}
	if err := m.UpdateSnapshotLimit(name, 4); err == nil {
		t.Fatal("quota reduction went below the immediate checkpoint count")
	}
	fresh, err := d.GetUserByName(name)
	if err != nil || fresh.SnapshotLimit != 5 {
		t.Fatal("rejected reduction or default change modified the account quota")
	}
	if err := runSnapshotDue(t, m, u); err != nil {
		t.Fatal(err)
	}
	snaps, _ = m.SnapshotList(name)
	for _, s := range snaps {
		if s.Automatic {
			if err := m.SnapshotPromote(name, s.Name); err == nil {
				t.Fatal("promotion exceeded fixed quota")
			}
		}
	}
	t.Log("verified independent real quotas, safe reductions/reservation, default isolation, promotion, disable/re-enable and create-before-delete rotation")
	// Empty this disposable history so the next section can verify disk data
	// and that restore truncates later checkpoints of both classifications.
	for _, s := range snaps {
		if err := m.SnapshotDelete(name, s.Name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.lx.ExecSH(name, "printf before > /root/vpsmgr-snapshot-test"); err != nil {
		t.Fatal(err)
	}
	if err := m.SnapshotCreate(name); err != nil {
		t.Fatal(err)
	}
	snaps, _ = m.SnapshotList(name)
	target := snaps[0].Name
	if _, err := m.lx.ExecSH(name, "printf after > /root/vpsmgr-snapshot-test"); err != nil {
		t.Fatal(err)
	}
	if err := runSnapshotDue(t, m, u); err != nil {
		t.Fatal(err)
	}
	if err := m.SnapshotCreate(name); err != nil {
		t.Fatal(err)
	}
	if err := m.SnapshotRestore(name, target); err != nil {
		t.Fatal(err)
	}
	if _, err := m.lx.ExecSH(name, "test \"$(cat /root/vpsmgr-snapshot-test)\" = before"); err != nil {
		t.Fatalf("disk data was not restored: %v", err)
	}
	snaps, err = m.SnapshotList(name)
	if err != nil || len(snaps) != 1 || snaps[0].Name != target {
		t.Fatalf("restore kept later checkpoints: count=%d, err=%v", len(snaps), err)
	}
	if _, _, err := m.ShareSnapshot(name, target); err != nil {
		t.Fatal(err)
	}
	if err := runSnapshotDue(t, m, u); err != nil {
		t.Fatal(err)
	}
	if err := m.SnapshotCreate(name); err != nil {
		t.Fatal(err)
	}
	snaps, _ = m.SnapshotList(name)
	var newestFixed string
	for _, s := range snaps {
		if !s.Automatic && (newestFixed == "" || snapshotCreateTime(s).After(snapshotTimeByName(snaps, newestFixed))) {
			newestFixed = s.Name
		}
	}
	if _, _, err := m.ShareSnapshot(name, newestFixed); err != nil {
		t.Fatal(err)
	}
	snaps, err = m.SnapshotList(name)
	if err != nil || len(snaps) != 1 || snaps[0].Name != newestFixed {
		t.Fatalf("share did not remove older fixed/automatic points: count=%d, err=%v", len(snaps), err)
	}
	t.Log("verified real disk restore, later-point truncation and fixed-only sharing with older-point cleanup")
}

// TestSnapshotIntegrationHTTP drives an actually running vps.service over HTTPS,
// including its background scheduler. No credentials or secret paths are logged.
func TestSnapshotIntegrationHTTP(t *testing.T) {
	if os.Getenv("VPSMGR_SNAPSHOT_HTTP") != "1" {
		t.Skip("requires explicit opt-in and the new vps.service running locally")
	}
	c, err := cfg.Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Panel.URLPath == "" {
		t.Fatal("requires an enabled user panel")
	}
	d, err := db.OpenWithSnapshotLimit(c.Panel.DB, c.Snapshots.Limit)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	m := New(c, d)
	name := fmt.Sprintf("snaphttp%xt", time.Now().UnixNano())
	limit := 1
	result, err := m.Add(name, AddOptions{CPU: 10, MemMB: 256, DiskGB: 2, SnapshotLimit: &limit})
	if err != nil {
		t.Fatalf("create HTTP test account: %v", err)
	}
	defer func() {
		if err := m.Del(name); err != nil {
			t.Errorf("remove HTTP test account: %v", err)
		}
	}()
	session, err := d.CreateSession(result.User.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(c.Panel.Listen)
	if err != nil {
		t.Fatal("cannot determine local HTTPS port")
	}
	origin := "https://localhost:" + port
	prefix := "/" + strings.Trim(c.Panel.URLPath, "/")
	client := &http.Client{
		// This is the installed panel's intentionally self-signed local TLS.
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
		Timeout:       2 * time.Minute,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer client.CloseIdleConnections()
	requestAs := func(prefix, path string, form url.Values, cookieName, token string) string {
		t.Helper()
		method, body := http.MethodGet, ""
		if form != nil {
			method, body = http.MethodPost, form.Encode()
		}
		req, err := http.NewRequest(method, origin+prefix+path, strings.NewReader(body))
		if err != nil {
			t.Fatal("build local panel request")
		}
		req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
		req.Header.Set("Origin", origin)
		if form != nil {
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal("local HTTPS panel request failed")
		}
		defer resp.Body.Close()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		if err != nil {
			t.Fatal("read local panel response")
		}
		want := http.StatusOK
		if form != nil {
			want = http.StatusFound
		}
		if resp.StatusCode != want {
			t.Fatalf("panel response status=%d, expected=%d", resp.StatusCode, want)
		}
		return string(b)
	}
	request := func(path string, form url.Values) string {
		t.Helper()
		return requestAs(prefix, path, form, "vpsmgr_session", session.Token)
	}
	var adminQuota func(int, bool)
	if c.Panel.AdminPath != "" {
		token, err := d.CreateAdminSession(1)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := d.DeleteAdminSession(token); err != nil {
				t.Errorf("remove integration admin session: %v", err)
			}
		}()
		adminPrefix := "/" + strings.Trim(c.Panel.AdminPath, "/")
		for _, lang := range []string{"en", "zh"} {
			body := requestAs(adminPrefix, "?lang="+lang, nil, "vpsmgr_admin_session", token)
			if !strings.Contains(body, "quotaSnapshots") || !strings.Contains(body, `name="snapshot_limit"`) {
				t.Fatal("deployed admin quota UI is missing")
			}
		}
		adminQuota = func(limit int, allowed bool) {
			t.Helper()
			before, err := d.GetUserByName(name)
			if err != nil {
				t.Fatal(err)
			}
			form := url.Values{"name": {name}, "cpu": {FormatCPU(before.CPU)},
				"mem": {fmt.Sprint(before.MemMB)}, "disk": {fmt.Sprint(before.DiskGB)},
				"bandwidth": {fmt.Sprint(before.BandwidthQuotaGB)}, "snapshot_limit": {fmt.Sprint(limit)}}
			requestAs(adminPrefix, "/user-quota", form, "vpsmgr_admin_session", token)
			want := before.SnapshotLimit
			if allowed {
				want = limit
			}
			after, err := d.GetUserByName(name)
			if err != nil || after.SnapshotLimit != want {
				t.Fatalf("admin quota=%d allowed=%v was not enforced", limit, allowed)
			}
		}
	} else {
		t.Log("administrator entrance disabled; its HTTPS forms are not tested")
	}
	cliQuota := func(limit int, allowed bool) {
		t.Helper()
		before, err := d.GetUserByName(name)
		if err != nil {
			t.Fatal(err)
		}
		// Discard output: successful quota commands contain live addresses.
		cmd := exec.Command("/usr/local/bin/vps", "quota", name, "--snapshots", fmt.Sprint(limit))
		if err := cmd.Run(); (err == nil) != allowed {
			t.Fatalf("installed CLI quota=%d allowed=%v returned unexpected status", limit, allowed)
		}
		want := before.SnapshotLimit
		if allowed {
			want = limit
		}
		after, err := d.GetUserByName(name)
		if err != nil || after.SnapshotLimit != want {
			t.Fatal("installed CLI persisted an unexpected quota")
		}
	}
	body := request("?lang=en", nil)
	if !strings.Contains(body, "snapScheduleForm") || !strings.Contains(body, "Automatic checkpoints") {
		t.Fatal("running panel did not render the new schedule UI")
	}
	request("/snapshot-schedule", url.Values{"enabled": {"1"}, "hours": {"1"}})
	schedule, err := m.SnapshotSchedule(name)
	if err != nil || schedule == nil || !schedule.Enabled || schedule.IntervalHours != 1 {
		t.Fatal("HTTPS enable did not persist the policy")
	}
	snaps, err := m.SnapshotList(name)
	if err != nil || len(snaps) != 1 || !snaps[0].Automatic || schedule.LastSuccessAt == 0 {
		t.Fatal("HTTPS enable did not immediately create the first automatic checkpoint")
	}
	initialName := snaps[0].Name
	request("/snapshot-schedule", url.Values{"enabled": {"1"}, "hours": {"1"}})
	unchanged, _ := m.SnapshotSchedule(name)
	if unchanged.NextRunAt != schedule.NextRunAt {
		t.Fatal("unchanged HTTPS interval postponed the schedule")
	}
	request("/snapshot-schedule", url.Values{"enabled": {"1"}, "hours": {"2"}})
	schedule, err = m.SnapshotSchedule(name)
	if err != nil || schedule.IntervalHours != 2 || schedule.NextRunAt < time.Now().Add(2*time.Hour-time.Minute).Unix() {
		t.Fatal("HTTPS interval edit did not restart the countdown")
	}
	snaps, err = m.SnapshotList(name)
	if err != nil || len(snaps) != 1 || snaps[0].Name != initialName {
		t.Fatal("HTTPS interval saves created an extra checkpoint")
	}
	request("/snapshot-schedule", url.Values{"enabled": {"1"}, "hours": {"1.5"}})
	unchanged, _ = m.SnapshotSchedule(name)
	if unchanged.IntervalHours != 2 || unchanged.NextRunAt != schedule.NextRunAt {
		t.Fatal("invalid HTTPS interval changed the policy")
	}
	if adminQuota != nil {
		adminQuota(0, false) // The immediate checkpoint and reservation block zero quota.
		adminQuota(2, true)
		adminQuota(1, true)
	}
	cliQuota(0, false)
	cliQuota(2, true)
	cliQuota(1, true)
	unchanged, err = m.SnapshotSchedule(name)
	if err != nil || unchanged == nil || !unchanged.Enabled || unchanged.NextRunAt != schedule.NextRunAt {
		t.Fatal("quota edits changed the automatic policy or deadline")
	}
	// Bring the deadline forward in the fixture, not the system clock. The
	// real service must notice and run its own scheduler within one scan.
	if err := d.SaveSnapshotSchedule(result.User.ID, true, 1, time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(75 * time.Second)
	for {
		schedule, err = m.SnapshotSchedule(name)
		if err != nil {
			t.Fatal(err)
		}
		snaps, err = m.SnapshotList(name)
		if err != nil {
			t.Fatal(err)
		}
		if schedule.LastError == "" && len(snaps) == 1 && snaps[0].Automatic && snaps[0].Name != initialName {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("service scheduler did not finish: %s", schedule.LastError)
		}
		time.Sleep(250 * time.Millisecond)
	}
	snaps, err = m.SnapshotList(name)
	if err != nil || len(snaps) != 1 || !snaps[0].Automatic || snaps[0].Name == initialName {
		t.Fatal("service scheduler did not rotate the immediate automatic checkpoint")
	}
	cliQuota(0, false) // The actual automatic checkpoint also blocks reduction.
	if adminQuota != nil {
		adminQuota(0, false)
	}
	body = request("?lang=zh", nil)
	if !strings.Contains(body, "转为固定") || !strings.Contains(body, "已启用") {
		t.Fatal("running Chinese panel did not show the automatic checkpoint")
	}
	if limit == 1 {
		request("/snapshot", url.Values{})
		request("/snapshot-promote", url.Values{"name": {snaps[0].Name}})
		after, _ := m.SnapshotList(name)
		if len(after) != 1 || !after[0].Automatic {
			t.Fatal("HTTPS quota enforcement failed")
		}
	}
	request("/snapshot-schedule", url.Values{"enabled": {"0"}})
	snaps, _ = m.SnapshotList(name)
	if len(snaps) != 1 || snaps[0].Automatic {
		t.Fatal("HTTPS disable did not preserve/fix the checkpoint")
	}
	if limit == 1 {
		request("/snapshot-schedule", url.Values{"enabled": {"1"}, "hours": {"1"}})
		schedule, _ = m.SnapshotSchedule(name)
		if schedule.Enabled {
			t.Fatal("HTTPS enabled without a free position")
		}
	}
	cliQuota(0, false) // Disabling preserves a fixed point, so zero is still unsafe.
	request("/snapshot-del", url.Values{"name": {snaps[0].Name}})
	after, err := m.SnapshotList(name)
	if err != nil || len(after) != 0 {
		t.Fatal("HTTPS deletion did not remove the fixture checkpoint")
	}
	cliQuota(0, true)
	cliQuota(1, true)
	if adminQuota != nil {
		adminQuota(0, true)
		adminQuota(1, true)
	}
	t.Log("verified deployed bilingual HTTPS user/admin UI, immediate first checkpoint, interval edits without creation, subsequent service rotation, CLI quotas, disable conversion and safe zero quota")
}
