package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func legacySnapshotQuota(limit int) *int { return &limit }

func TestMigrateV22SnapshotLimits(t *testing.T) {
	for _, version := range []int{20, 21} {
		for _, limit := range []int{0, 5} {
			t.Run(fmt.Sprintf("v%d/legacy%d", version, limit), func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "legacy.db")
				d, err := Open(path)
				if err != nil {
					t.Fatal(err)
				}
				for i := 1; i <= 2; i++ {
					snapshotTestUser(t, d, i)
				}
				if err := d.SetSnapshotShare(1, "legacy-share", "fixed-point"); err != nil {
					t.Fatal(err)
				}
				if version == 21 {
					if err := d.SaveSnapshotSchedule(1, true, 6, 100); err != nil {
						t.Fatal(err)
					}
					if err := d.BeginAutomaticSnapshot(1, "auto-point", 100, 200); err != nil {
						t.Fatal(err)
					}
				}
				for _, stmt := range []string{
					`ALTER TABLE users DROP COLUMN snapshot_limit`,
					`DELETE FROM schema_migrations WHERE version=22`,
				} {
					if _, err := d.sql.Exec(stmt); err != nil {
						t.Fatal(err)
					}
				}
				if version == 20 {
					for _, stmt := range []string{
						`DROP TABLE automatic_snapshots`,
						`DROP TABLE snapshot_schedules`,
						`DELETE FROM schema_migrations WHERE version=21`,
					} {
						if _, err := d.sql.Exec(stmt); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := d.Close(); err != nil {
					t.Fatal(err)
				}

				d, err = OpenWithLegacySnapshotQuota(path, &limit)
				if err != nil {
					t.Fatal(err)
				}
				users, err := d.ListUsers()
				if err != nil || len(users) != 2 {
					t.Fatalf("users = %+v, err=%v", users, err)
				}
				for _, u := range users {
					if u.SnapshotLimit != limit {
						t.Fatalf("user %d snapshot limit = %d, want %d", u.ID, u.SnapshotLimit, limit)
					}
				}
				if err := d.UpdateSnapshotLimit(2, 3); err != nil {
					t.Fatal(err)
				}
				if err := d.Close(); err != nil {
					t.Fatal(err)
				}

				// Neither an inherited quota nor an explicitly edited quota may be
				// reseeded when a different legacy migration source is supplied.
				d, err = OpenWithLegacySnapshotQuota(path, legacySnapshotQuota(9))
				if err != nil {
					t.Fatal(err)
				}
				defer d.Close()
				for id, want := range map[int64]int{1: limit, 2: 3} {
					u, err := d.GetUserByID(id)
					if err != nil || u.SnapshotLimit != want {
						t.Fatalf("reopened user %d = %+v, err=%v; want limit %d", id, u, err, want)
					}
				}
				id := snapshotTestUser(t, d, 3)
				u, err := d.GetUserByID(id)
				if err != nil || u.SnapshotLimit != 1 {
					t.Fatalf("new user = %+v, err=%v; want fixed limit 1", u, err)
				}
				share, err := d.GetSnapshotShare(1)
				if err != nil || share == nil || share.Snapshot != "fixed-point" || share.Code != "legacy-share" {
					t.Fatalf("legacy share = %+v, err=%v", share, err)
				}
				if version == 21 {
					requireSnapshotSchedule(t, d, SnapshotSchedule{UserID: 1, Enabled: true, IntervalHours: 6, NextRunAt: 200, LastRunAt: 100})
					requireAutomaticSnapshots(t, d, 1, "auto-point")
				} else {
					if s, err := d.GetSnapshotSchedule(1); err != nil || s != nil {
						t.Fatalf("unexpected schedule = %+v, err=%v", s, err)
					}
					requireAutomaticSnapshots(t, d, 1)
				}
				applied, err := d.appliedMigrations()
				if err != nil || !applied[21] || !applied[22] {
					t.Fatalf("migration versions = %v, err=%v", applied, err)
				}
			})
		}
	}
}

func TestSnapshotLimitCreationAndScans(t *testing.T) {
	for _, tc := range []struct {
		name  string
		quota *int
	}{{"absent", nil}, {"zero", legacySnapshotQuota(0)}, {"five", legacySnapshotQuota(5)}} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := OpenWithLegacySnapshotQuota(filepath.Join(t.TempDir(), "users.db"), tc.quota)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			simple, err := d.CreateUser("simple", "hash", "192.0.2.1", 1, 30001, 10100, 1, 128, 1)
			if err != nil {
				t.Fatal(err)
			}
			users := []*User{simple}
			for i, explicit := range [][]int{nil, {0}, {4}} {
				u, err := d.CreateUserFull(fmt.Sprintf("full%d", i), "hash", "192.0.2.2", i+2, 30002+i, 10200+i*100,
					1, 128, 1, 25, StatusCreating, "", 0, "", "", explicit...)
				if err != nil {
					t.Fatal(err)
				}
				users = append(users, u)
				bw, err := d.GetBandwidth(u.ID)
				if err != nil || bw == nil || u.BandwidthQuotaGB != 25 {
					t.Fatalf("full user bandwidth = %+v, user=%+v, err=%v", bw, u, err)
				}
			}
			listed, err := d.ListUsers()
			if err != nil || len(listed) != len(users) {
				t.Fatalf("ListUsers = %+v, err=%v", listed, err)
			}
			for i, want := range []int{1, 1, 0, 4} {
				u := users[i]
				byID, err := d.GetUserByID(u.ID)
				if err != nil {
					t.Fatal(err)
				}
				byName, err := d.GetUserByName(u.Name)
				if err != nil {
					t.Fatal(err)
				}
				for _, got := range []*User{u, byID, byName, listed[i]} {
					if got.SnapshotLimit != want {
						t.Fatalf("user %s snapshot limit = %d, want %d", got.Name, got.SnapshotLimit, want)
					}
				}
			}
		})
	}
	// Opening without legacy migration metadata uses the fixed quota of one.
	d := snapshotTestDB(t)
	id := snapshotTestUser(t, d, 1)
	u, err := d.GetUserByID(id)
	if err != nil || u.SnapshotLimit != 1 {
		t.Fatalf("Open default user = %+v, err=%v", u, err)
	}
}

func TestSnapshotLimitValidationAndConstraints(t *testing.T) {
	d := snapshotTestDB(t)
	id := snapshotTestUser(t, d, 1)
	for _, limits := range [][]int{{-1}, {0, 1}, {1, -1}} {
		u, err := d.CreateUserFull("invalid", "hash", "192.0.2.2", 2, 30002, 10200,
			1, 128, 1, 25, "", "", 0, "", "", limits...)
		if err == nil || u != nil {
			t.Fatalf("CreateUserFull(%v) = %+v, %v; want validation error", limits, u, err)
		}
	}
	if err := d.UpdateSnapshotLimit(id, -1); err == nil {
		t.Fatal("UpdateSnapshotLimit accepted a negative quota")
	}
	for _, stmt := range []string{
		`UPDATE users SET snapshot_limit=-1 WHERE id=1`,
		`UPDATE users SET snapshot_limit=NULL WHERE id=1`,
		`INSERT INTO users(name,pass_hash,idx,ip,ssh_port,start_port,created_at,snapshot_limit)
		 VALUES('bad','hash',2,'192.0.2.2',30002,10200,'2026-01-01T00:00:00Z',-1)`,
	} {
		if _, err := d.sql.Exec(stmt); err == nil {
			t.Fatalf("constraint accepted %s", stmt)
		}
	}
	users, err := d.ListUsers()
	if err != nil || len(users) != 1 || users[0].SnapshotLimit != 1 {
		t.Fatalf("validation changed users = %+v, err=%v", users, err)
	}
	var n int
	if err := d.sql.QueryRow(`SELECT count(*) FROM bandwidth`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("invalid creation wrote bandwidth rows = %d, err=%v", n, err)
	}
}

func TestSnapshotLimitUpdateOnlyChangesQuota(t *testing.T) {
	d := snapshotTestDB(t)
	id := snapshotTestUser(t, d, 1)
	if err := d.SaveSnapshotSchedule(id, true, 6, 100); err != nil {
		t.Fatal(err)
	}
	if err := d.BeginAutomaticSnapshot(id, "automatic", 100, 200); err != nil {
		t.Fatal(err)
	}
	if err := d.SetSnapshotShare(id, "share", "automatic"); err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int{7, 0} {
		if err := d.UpdateSnapshotLimit(id, limit); err != nil {
			t.Fatal(err)
		}
		u, err := d.GetUserByID(id)
		if err != nil || u.SnapshotLimit != limit {
			t.Fatalf("updated user = %+v, err=%v", u, err)
		}
		requireSnapshotSchedule(t, d, SnapshotSchedule{UserID: id, Enabled: true, IntervalHours: 6, NextRunAt: 200, LastRunAt: 100})
		requireAutomaticSnapshots(t, d, id, "automatic")
		share, err := d.GetSnapshotShare(id)
		if err != nil || share == nil || share.Code != "share" {
			t.Fatalf("quota update changed share = %+v, err=%v", share, err)
		}
	}
}

func TestSnapshotLimitCreateUserFullRollback(t *testing.T) {
	d := snapshotTestDB(t)
	if _, err := d.sql.Exec(`CREATE TRIGGER fail_bandwidth BEFORE INSERT ON bandwidth
		BEGIN SELECT RAISE(ABORT, 'injected bandwidth failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateUserFull("rollback", "hash", "192.0.2.1", 1, 30001, 10100,
		1, 128, 1, 25, "", "", 0, "", "", 7); err == nil {
		t.Fatal("creation ignored bandwidth failure")
	}
	for _, table := range []string{"users", "bandwidth"} {
		var n int
		if err := d.sql.QueryRow(`SELECT count(*) FROM ` + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s rows after rollback = %d, err=%v", table, n, err)
		}
	}
}

func TestOpenNegativeSnapshotLimitDoesNotTouchDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "untouched.db")
	if d, err := OpenWithLegacySnapshotQuota(path, legacySnapshotQuota(-1)); err == nil || d != nil {
		t.Fatalf("negative default = %v, %v", d, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("negative default created database: %v", err)
	}
	content := "not a database"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if d, err := OpenWithLegacySnapshotQuota(path, legacySnapshotQuota(-1)); err == nil || d != nil {
		t.Fatalf("negative default on existing file = %v, %v", d, err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != content {
		t.Fatalf("negative default changed file contents: %q, %v", got, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("negative default changed file permissions: %v, %v", info, err)
	}
}

func TestMigrateV22SeedFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollback.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	snapshotTestUser(t, d, 1)
	for _, stmt := range []string{
		`ALTER TABLE users DROP COLUMN snapshot_limit`,
		`DELETE FROM schema_migrations WHERE version=22`,
		`CREATE TRIGGER fail_snapshot_seed BEFORE UPDATE ON users
		 BEGIN SELECT RAISE(ABORT, 'injected seed failure'); END`,
	} {
		if _, err := d.sql.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if d, err := OpenWithLegacySnapshotQuota(path, legacySnapshotQuota(7)); err == nil || d != nil || !strings.Contains(err.Error(), "injected seed failure") {
		t.Fatalf("failed migration = %v, %v", d, err)
	}
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if hasColumn(t, raw, "users", "snapshot_limit") {
		t.Fatal("failed migration left snapshot_limit column")
	}
	var n int
	if err := raw.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version=22`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("v22 recorded after failure: count=%d, err=%v", n, err)
	}
	if _, err := raw.Exec(`DROP TRIGGER fail_snapshot_seed`); err != nil {
		t.Fatal(err)
	}
	d, err = OpenWithLegacySnapshotQuota(path, legacySnapshotQuota(0))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	u, err := d.GetUserByID(1)
	if err != nil || u.SnapshotLimit != 0 {
		t.Fatalf("retry user = %+v, err=%v; want limit 0", u, err)
	}
}

func TestSnapshotLimitOpenRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`INSERT INTO schema_migrations(version,applied_at) VALUES(?,?)`, schemaVersion+1, now()); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if d, err := OpenWithLegacySnapshotQuota(path, legacySnapshotQuota(7)); err == nil || d != nil || !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("future schema = %v, %v", d, err)
	}
}
