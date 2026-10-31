package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

func snapshotTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "snapshots.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func snapshotTestUser(t *testing.T, d *DB, idx int) int64 {
	t.Helper()
	u, err := d.CreateUser(fmt.Sprintf("user%d", idx), "test-hash", fmt.Sprintf("192.0.2.%d", idx),
		idx, 30000+idx, 10000+idx*100, 1, 128, 1)
	if err != nil {
		t.Fatal(err)
	}
	return u.ID
}

func requireSnapshotSchedule(t *testing.T, d *DB, want SnapshotSchedule) {
	t.Helper()
	got, err := d.GetSnapshotSchedule(want.UserID)
	if err != nil || got == nil || *got != want {
		t.Fatalf("schedule = %+v, err=%v; want %+v", got, err, want)
	}
}

func requireAutomaticSnapshots(t *testing.T, d *DB, userID int64, names ...string) {
	t.Helper()
	want := make(map[string]bool)
	for _, name := range names {
		want[name] = true
	}
	got, err := d.AutomaticSnapshots(userID)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("automatic snapshots = %v, err=%v; want %v", got, err, want)
	}
}

func TestMigrateV21PreservesV20Data(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v20.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	// Build a genuine v20 schema without creating any v21 objects first.
	for _, m := range migrations {
		if m.version > 20 {
			break
		}
		tx, err := raw.Begin()
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range m.stmts {
			if _, err := tx.Exec(stmt); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
		}
		if step := migrationSteps[m.version]; step != nil {
			if err := step(tx); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
		}
		if m.version > 1 {
			if _, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(?,?)`, m.version, now()); err != nil {
				tx.Rollback()
				t.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	for _, stmt := range []string{
		`INSERT INTO users(id,name,pass_hash,idx,ip,ssh_port,start_port,created_at,ipv6_index,ipv6_extra_block)
		 VALUES(1,'legacy','test-hash',1,'192.0.2.1',30001,10100,'2026-01-01T00:00:00Z',42,'2001:db8:1::/64')`,
		`INSERT INTO snapshot_shares(code,user_id,snapshot,created_at) VALUES('old-share',1,'old-point','2026-01-01T00:00:00Z')`,
		`INSERT INTO settings(key,value) VALUES('migration-test','preserved')`,
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	u, err := d.GetUserByID(1)
	if err != nil || u == nil || u.Name != "legacy" || u.IP != "192.0.2.1" || u.IPv6Index != 42 || u.IPv6ExtraBlock != "2001:db8:1::/64" {
		t.Fatalf("legacy user = %+v, err=%v", u, err)
	}
	share, err := d.GetSnapshotShare(1)
	if err != nil || share == nil || share.Snapshot != "old-point" || share.Code != "old-share" {
		t.Fatalf("legacy share = %+v, err=%v", share, err)
	}
	if value, ok, err := d.GetSetting("migration-test"); err != nil || !ok || value != "preserved" {
		t.Fatalf("legacy setting = %q, %v, %v", value, ok, err)
	}
	if s, err := d.GetSnapshotSchedule(1); err != nil || s != nil {
		t.Fatalf("legacy user unexpectedly scheduled: %+v, %v", s, err)
	}
	requireAutomaticSnapshots(t, d, 1)
	applied, err := d.appliedMigrations()
	if err != nil || !applied[21] {
		t.Fatalf("v21 not applied: %v, %v", applied, err)
	}
}

func TestSnapshotScheduleAttemptFinishAndPersistence(t *testing.T) {
	d := snapshotTestDB(t)
	id := snapshotTestUser(t, d, 1)
	if s, err := d.GetSnapshotSchedule(id); err != nil || s != nil {
		t.Fatalf("missing schedule = %+v, %v", s, err)
	}
	if err := d.SaveSnapshotSchedule(id, true, 24, 100); err != nil {
		t.Fatal(err)
	}
	want := SnapshotSchedule{UserID: id, Enabled: true, IntervalHours: 24, NextRunAt: 100}
	requireSnapshotSchedule(t, d, want)
	if err := d.RecordSnapshotAttempt(id, 100, 200, "quota exceeded"); err != nil {
		t.Fatal(err)
	}
	want.LastRunAt, want.NextRunAt, want.LastError = 100, 200, "quota exceeded"
	requireSnapshotSchedule(t, d, want)
	requireAutomaticSnapshots(t, d, id)
	if err := d.BeginAutomaticSnapshot(id, "auto-one", 200, 300); err != nil {
		t.Fatal(err)
	}
	want.LastRunAt, want.NextRunAt, want.LastError = 200, 300, ""
	requireSnapshotSchedule(t, d, want)
	// Intent exists even though no runtime snapshot creation has happened.
	requireAutomaticSnapshots(t, d, id, "auto-one")
	if err := d.FinishAutomaticSnapshot(id, 205, ""); err != nil {
		t.Fatal(err)
	}
	want.LastSuccessAt = 205
	requireSnapshotSchedule(t, d, want)
	for _, successAt := range []int64{0, -1} {
		if err := d.FinishAutomaticSnapshot(id, successAt, "create failed"); err != nil {
			t.Fatal(err)
		}
		want.LastError = "create failed"
		requireSnapshotSchedule(t, d, want)
	}
	if err := d.RecordSnapshotAttempt(id, 300, 400, "not eligible"); err != nil {
		t.Fatal(err)
	}
	want.LastRunAt, want.NextRunAt, want.LastError = 300, 400, "not eligible"
	requireSnapshotSchedule(t, d, want)
	if err := d.SaveSnapshotSchedule(id, true, 12, 500); err != nil {
		t.Fatal(err)
	}
	want.IntervalHours, want.NextRunAt = 12, 500
	requireSnapshotSchedule(t, d, want)

	path := d.path
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	requireSnapshotSchedule(t, reopened, want)
	requireAutomaticSnapshots(t, reopened, id, "auto-one")
}

func TestSnapshotSchedulesDueOrder(t *testing.T) {
	d := snapshotTestDB(t)
	var ids []int64
	for i := 1; i <= 5; i++ {
		ids = append(ids, snapshotTestUser(t, d, i))
	}
	for i, next := range []int64{100, 100, 50, 101, 0} {
		if err := d.SaveSnapshotSchedule(ids[i], i != 4, 1, next); err != nil {
			t.Fatal(err)
		}
	}
	for _, now := range []int64{49, 50, 100, 101} {
		got, err := d.ListDueSnapshotSchedules(now)
		if err != nil {
			t.Fatal(err)
		}
		var gotIDs []int64
		for _, s := range got {
			if !s.Enabled || s.IntervalHours != 1 || s.NextRunAt > now {
				t.Fatalf("unexpected due schedule: %+v", s)
			}
			gotIDs = append(gotIDs, s.UserID)
		}
		var want []int64
		if now >= 50 {
			want = append(want, ids[2])
		}
		if now >= 100 {
			want = append(want, ids[0], ids[1])
		}
		if now >= 101 {
			want = append(want, ids[3])
		}
		if !reflect.DeepEqual(gotIDs, want) {
			t.Fatalf("due at %d = %v, want %v", now, gotIDs, want)
		}
	}
}

func TestSnapshotScheduleDisableAtomicAndBeginChecks(t *testing.T) {
	d := snapshotTestDB(t)
	id := snapshotTestUser(t, d, 1)
	other := snapshotTestUser(t, d, 2)
	if err := d.BeginAutomaticSnapshot(id, "missing", 1, 2); err == nil {
		t.Fatal("begin accepted a missing policy")
	}
	requireAutomaticSnapshots(t, d, id)
	for _, uid := range []int64{id, other} {
		if err := d.SaveSnapshotSchedule(uid, true, 1, 100); err != nil {
			t.Fatal(err)
		}
		if err := d.BeginAutomaticSnapshot(uid, "same-name", 100, 200); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.FinishAutomaticSnapshot(id, 105, "previous result"); err != nil {
		t.Fatal(err)
	}
	want := SnapshotSchedule{UserID: id, Enabled: true, IntervalHours: 1, NextRunAt: 200,
		LastRunAt: 100, LastSuccessAt: 105, LastError: "previous result"}
	if err := d.BeginAutomaticSnapshot(id, "same-name", 200, 300); err == nil {
		t.Fatal("duplicate intent accepted")
	}
	requireSnapshotSchedule(t, d, want) // failed intent rolls the policy update back
	if _, err := d.sql.Exec(`CREATE TRIGGER fail_automatic_delete BEFORE DELETE ON automatic_snapshots
		BEGIN SELECT RAISE(ABORT, 'injected deletion failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveSnapshotSchedule(id, false, 2, 300); err == nil {
		t.Fatal("disable ignored metadata deletion error")
	}
	requireSnapshotSchedule(t, d, want) // disabling and conversion must both roll back
	requireAutomaticSnapshots(t, d, id, "same-name")
	if _, err := d.sql.Exec(`DROP TRIGGER fail_automatic_delete`); err != nil {
		t.Fatal(err)
	}
	if err := d.SaveSnapshotSchedule(id, false, 1, 300); err != nil {
		t.Fatal(err)
	}
	want.Enabled, want.NextRunAt = false, 300
	requireSnapshotSchedule(t, d, want)
	requireAutomaticSnapshots(t, d, id)
	requireAutomaticSnapshots(t, d, other, "same-name")
	if err := d.BeginAutomaticSnapshot(id, "disabled", 300, 400); err == nil {
		t.Fatal("begin accepted a disabled policy")
	}
	requireSnapshotSchedule(t, d, want)
	requireAutomaticSnapshots(t, d, id)
	if err := d.SaveSnapshotSchedule(id, true, 1, 400); err != nil {
		t.Fatal(err)
	}
	requireAutomaticSnapshots(t, d, id) // enabling cannot reclassify old fixed points
}

func TestAutomaticSnapshotMetadataAndReset(t *testing.T) {
	d := snapshotTestDB(t)
	id := snapshotTestUser(t, d, 1)
	other := snapshotTestUser(t, d, 2)
	for _, uid := range []int64{id, other} {
		if err := d.SaveSnapshotSchedule(uid, true, 6, 100); err != nil {
			t.Fatal(err)
		}
		for i, name := range []string{"one", "two"} {
			if err := d.BeginAutomaticSnapshot(uid, name, int64(100+i), int64(200+i)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := d.DeleteAutomaticSnapshot(id, "one"); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteAutomaticSnapshot(id, "absent"); err != nil {
		t.Fatal(err)
	}
	requireAutomaticSnapshots(t, d, id, "two")
	requireAutomaticSnapshots(t, d, other, "one", "two")
	if err := d.FinishAutomaticSnapshot(id, 110, "result"); err != nil {
		t.Fatal(err)
	}
	want := SnapshotSchedule{UserID: id, Enabled: true, IntervalHours: 6, NextRunAt: 201,
		LastRunAt: 101, LastSuccessAt: 110, LastError: "result"}
	if _, err := d.sql.Exec(`CREATE TRIGGER fail_schedule_reset BEFORE UPDATE OF next_run_at ON snapshot_schedules
		BEGIN SELECT RAISE(ABORT, 'injected reset failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := d.ResetSnapshotSchedule(id, 500); err == nil {
		t.Fatal("reset ignored update error")
	}
	requireAutomaticSnapshots(t, d, id, "two")
	requireSnapshotSchedule(t, d, want)
	if _, err := d.sql.Exec(`DROP TRIGGER fail_schedule_reset`); err != nil {
		t.Fatal(err)
	}
	if err := d.ResetSnapshotSchedule(id, 500); err != nil {
		t.Fatal(err)
	}
	want.NextRunAt = 500
	requireSnapshotSchedule(t, d, want)
	requireAutomaticSnapshots(t, d, id)
	requireAutomaticSnapshots(t, d, other, "one", "two")
	if err := d.ClearAutomaticSnapshots(other); err != nil {
		t.Fatal(err)
	}
	requireAutomaticSnapshots(t, d, other)
	if err := d.SaveSnapshotSchedule(id, false, 6, 600); err != nil {
		t.Fatal(err)
	}
	if err := d.ResetSnapshotSchedule(id, 700); err != nil {
		t.Fatal(err)
	}
	want.Enabled, want.NextRunAt = false, 600
	requireSnapshotSchedule(t, d, want)
	if err := d.ResetSnapshotSchedule(99999, 700); err != nil {
		t.Fatal(err)
	}
	if s, err := d.GetSnapshotSchedule(99999); err != nil || s != nil {
		t.Fatalf("reset created a policy: %+v, %v", s, err)
	}
}

func TestSnapshotScheduleConstraintsAndCascade(t *testing.T) {
	d := snapshotTestDB(t)
	id := snapshotTestUser(t, d, 1)
	other := snapshotTestUser(t, d, 2)
	for _, hours := range []int{0, -1} {
		if err := d.SaveSnapshotSchedule(id, true, hours, 100); err == nil {
			t.Fatalf("accepted invalid interval %d", hours)
		}
	}
	if err := d.SaveSnapshotSchedule(99999, true, 1, 100); err == nil {
		t.Fatal("schedule accepted nonexistent user")
	}
	if _, err := d.sql.Exec(`INSERT INTO automatic_snapshots(user_id,snapshot) VALUES(99999,'orphan')`); err == nil {
		t.Fatal("metadata accepted nonexistent user")
	}
	if _, err := d.sql.Exec(`INSERT INTO snapshot_schedules(user_id,enabled,interval_hours,next_run_at) VALUES(?,2,1,100)`, id); err == nil {
		t.Fatal("accepted invalid boolean")
	}
	for _, uid := range []int64{id, other} {
		if err := d.SaveSnapshotSchedule(uid, true, 1, 100); err != nil {
			t.Fatal(err)
		}
		if err := d.BeginAutomaticSnapshot(uid, "one", 100, 200); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.DeleteUser(id); err != nil {
		t.Fatal(err)
	}
	if s, err := d.GetSnapshotSchedule(id); err != nil || s != nil {
		t.Fatalf("schedule did not cascade: %+v, %v", s, err)
	}
	requireAutomaticSnapshots(t, d, id)
	requireAutomaticSnapshots(t, d, other, "one")
	requireSnapshotSchedule(t, d, SnapshotSchedule{UserID: other, Enabled: true, IntervalHours: 1, NextRunAt: 200, LastRunAt: 100})
}
