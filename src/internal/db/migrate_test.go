package db

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// TestMigrateUpgradesLegacyDatabase simulates a pre-migration database: the
// original schema with users rows but NO schema_migrations table (the old code
// created tables directly). Opening it must apply v2 (users.status) and leave
// existing users 'ready'.
func TestMigrateUpgradesLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")

	legacy := []string{
		`CREATE TABLE users(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			pass_hash TEXT NOT NULL,
			idx INTEGER UNIQUE NOT NULL,
			ip TEXT NOT NULL,
			ssh_port INTEGER UNIQUE NOT NULL,
			start_port INTEGER NOT NULL,
			init_script TEXT NOT NULL DEFAULT '',
			bandwidth_quota_gb INTEGER NOT NULL DEFAULT 0,
			cpu INTEGER NOT NULL DEFAULT 10,
			mem_mb INTEGER NOT NULL DEFAULT 1024,
			disk_gb INTEGER NOT NULL DEFAULT 10,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE domains(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			domain TEXT UNIQUE NOT NULL,
			proxy_protocol INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE sessions(
			token TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			expires_at INTEGER NOT NULL
		)`,
		`CREATE TABLE bandwidth(
			user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			period TEXT NOT NULL,
			upload_bytes INTEGER NOT NULL DEFAULT 0,
			download_bytes INTEGER NOT NULL DEFAULT 0,
			last_rx INTEGER NOT NULL DEFAULT 0,
			last_tx INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE settings(key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range legacy {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("create legacy schema: %v", err)
		}
	}
	for _, u := range []struct {
		name, ip string
		idx, ssh int
	}{
		{"alice", "10.115.0.2", 1, 30001},
		{"bob", "10.115.0.3", 2, 30002},
	} {
		if _, err := db.Exec(
			`INSERT INTO users(name, pass_hash, idx, ip, ssh_port, start_port, cpu, mem_mb, disk_gb, created_at)
			 VALUES(?, 'h', ?, ?, ?, ?, 1, 1024, 10, '2026-01-01T00:00:00Z')`,
			u.name, u.idx, u.ip, u.ssh, 10000+u.idx); err != nil {
			t.Fatalf("seed legacy user %s: %v", u.name, err)
		}
	}
	db.Close()

	// Open runs the migrations.
	d, err := Open(path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer d.Close()

	u, err := d.GetUserByName("alice")
	if err != nil {
		t.Fatalf("legacy user lost: %v", err)
	}
	if u.Status != StatusReady {
		t.Errorf("legacy user status = %q, want %q", u.Status, StatusReady)
	}
	// v19 seeds the block index of every account with the value the old
	// username-derived scheme gave it (sha256(name)[:4]), so no address moves
	// when the value stops being derived.
	for name, want := range map[string]int64{"alice": 0x2bd806c9, "bob": 0x81b637d8} {
		got, err := d.GetUserByName(name)
		if err != nil {
			t.Fatalf("legacy user %s lost: %v", name, err)
		}
		if got.IPv6Index != want {
			t.Errorf("%s ipv6 index = %#x, want %#x", name, got.IPv6Index, want)
		}
	}
	applied, err := d.appliedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if !applied[schemaVersion] {
		t.Errorf("schema version %d not recorded; applied=%v", schemaVersion, applied)
	}
}

// TestMigrateRefusesGapInVersions: applied versions must form an unbroken run.
// A missing row means the schema silently skipped a step, which would surface
// much later as an obscure "no such column"; Open must refuse instead.
func TestMigrateRefusesGapInVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gap.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`DELETE FROM schema_migrations WHERE version = 3`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil ||
		!strings.Contains(err.Error(), "missing migration v3") {
		t.Fatalf("gap not detected: %v", err)
	}

	// Restoring the row makes the database openable again.
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(
		`INSERT INTO schema_migrations(version, applied_at) VALUES(3, '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	d3, err := Open(path)
	if err != nil {
		t.Fatalf("open after the row was restored: %v", err)
	}
	d3.Close()
}

// TestMigrationIdempotent opens the same DB twice — the second Open must not
// re-apply migrations or fail.
func TestMigrationIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idem.db")
	d1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d1.CreateUser("bob", "h", "10.115.0.3", 2, 30002, 10100, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	d1.Close()

	d2, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer d2.Close()
	u, err := d2.GetUserByName("bob")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != StatusReady {
		t.Errorf("bob status = %q, want ready", u.Status)
	}
}

// TestUserStatusRoundTrip exercises the lifecycle status transitions.
func TestUserStatusRoundTrip(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	u, err := d.CreateUserFull("carol", "h", "10.115.0.4", 3, 30003, 10200, 1, 1024, 10, 0, StatusCreating, "", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if u.Status != StatusCreating {
		t.Errorf("initial status = %q, want creating", u.Status)
	}
	if err := d.UpdateUserStatus(u.ID, StatusReady); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetUserByID(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusReady {
		t.Errorf("status after update = %q, want ready", got.Status)
	}
	if err := d.UpdateUserStatus(u.ID, StatusFailed); err != nil {
		t.Fatal(err)
	}
	got, _ = d.GetUserByID(u.ID)
	if got.Status != StatusFailed {
		t.Errorf("status after fail = %q, want failed", got.Status)
	}
}

// TestMigrateStepFailureRollsBack: v19 adds a column and then seeds it from Go
// code, both inside the migration's transaction. If the seeding fails, nothing
// may be left behind — no column, no index, no version row — so the next start
// retries the migration from a clean v18 schema instead of finding half of it
// applied (which would fail forever with "duplicate column name").
func TestMigrateStepFailureRollsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stepfail.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	// Back to the v18 state, schema included.
	for _, s := range []string{
		`DELETE FROM schema_migrations WHERE version = 19`,
		`DROP INDEX IF EXISTS idx_users_ipv6_index`,
		`ALTER TABLE users DROP COLUMN ipv6_index`,
	} {
		if _, err := d.sql.Exec(s); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()

	boom := errors.New("seeding failed")
	orig := migrationSteps[19]
	migrationSteps[19] = func(*sql.Tx) error { return boom }
	defer func() { migrationSteps[19] = orig }()

	if _, err := Open(path); !errors.Is(err, boom) {
		t.Fatalf("Open with a failing v19 step: %v, want %v", err, boom)
	}

	// The schema is exactly as it was: nothing half-applied, data intact.
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, col := range []string{"ipv6_index"} {
		if hasColumn(t, raw, "users", col) {
			t.Errorf("column %s survived the rollback", col)
		}
	}
	var n int
	if err := raw.QueryRow(`SELECT count(*) FROM schema_migrations WHERE version = 19`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("v19 recorded despite the failure")
	}
	if err := raw.QueryRow(`SELECT count(*) FROM users`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("users = %d, want 1 (the rollback lost data)", n)
	}

	// With the step restored the migration runs again and completes.
	migrationSteps[19] = orig
	d2, err := Open(path)
	if err != nil {
		t.Fatalf("retry after a rolled-back attempt: %v", err)
	}
	defer d2.Close()
	u, err := d2.GetUserByName("alice")
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(0x2bd806c9); u.IPv6Index != want {
		t.Errorf("index after retry = %#x, want %#x", u.IPv6Index, want)
	}
	// The same check that said "absent" before the retry says "present" now, so
	// its verdict above was not vacuous.
	if !hasColumn(t, raw, "users", "ipv6_index") {
		t.Errorf("column still missing after the migration succeeded")
	}
}

// hasColumn reports whether a table has a column — used to assert a
// migration's structural effect, or its absence after a rollback.
func hasColumn(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid, notnull, pk int
			name, ctype      string
			dflt             sql.NullString
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return false
}
