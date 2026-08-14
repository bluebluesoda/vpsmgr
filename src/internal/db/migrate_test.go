package db

import (
	"database/sql"
	"path/filepath"
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
	if _, err := db.Exec(
		`INSERT INTO users(name, pass_hash, idx, ip, ssh_port, start_port, cpu, mem_mb, disk_gb, created_at)
		 VALUES('alice','h',1,'10.115.0.2',30001,10000,1,1024,10,'2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("seed legacy user: %v", err)
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
	applied, err := d.appliedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if !applied[schemaVersion] {
		t.Errorf("schema version %d not recorded; applied=%v", schemaVersion, applied)
	}
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
	u, err := d.CreateUserFull("carol", "h", "10.115.0.4", 3, 30003, 10200, 1, 1024, 10, 0, StatusCreating)
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
