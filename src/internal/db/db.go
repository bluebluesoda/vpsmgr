package db

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	sql *sql.DB
}

func Open(path string) (*DB, error) {
	// The DB holds bcrypt hashes and session-token hashes, but permissions
	// must still be explicit: a 0644 file (default umask) lets any local user
	// read it. 0600 keeps it private to the owning (vps) user.
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600); err != nil {
		return nil, fmt.Errorf("create db %s: %w", path, err)
	} else {
		if err := f.Chmod(0o600); err != nil {
			f.Close()
			return nil, fmt.Errorf("chmod db %s: %w", path, err)
		}
		if err := f.Close(); err != nil {
			return nil, err
		}
	}
	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	s, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s.SetMaxOpenConns(1)
	d := &DB{sql: s}
	if err := d.migrate(); err != nil {
		s.Close()
		return nil, err
	}
	// WAL journal and shared-memory files are created lazily alongside the DB;
	// make sure any that already exist are equally private.
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Chmod(path+suffix, 0o600)
	}
	return d, nil
}

func (d *DB) Close() error { return d.sql.Close() }

// schemaVersion is the current schema version. Every migration in
// migrations must be applied in order; Open refuses to start on a database
// whose version is newer than this binary understands (downgrade protection).
const schemaVersion = 3

// migrations are applied in order, each inside its own transaction. v1 is the
// original schema (baseline); later versions only add/alter, never drop.
var migrations = []struct {
	version int
	stmts   []string
}{
	{1, []string{
		`CREATE TABLE IF NOT EXISTS users(
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
		`CREATE TABLE IF NOT EXISTS domains(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			domain TEXT UNIQUE NOT NULL,
			proxy_protocol INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS sessions(
			token TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			expires_at INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS bandwidth(
			user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			period TEXT NOT NULL,
			upload_bytes INTEGER NOT NULL DEFAULT 0,
			download_bytes INTEGER NOT NULL DEFAULT 0,
			last_rx INTEGER NOT NULL DEFAULT 0,
			last_tx INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS audit_log(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			actor TEXT NOT NULL,
			action TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS settings(
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS schema_migrations(
			version INTEGER PRIMARY KEY,
			applied_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_domains_user ON domains(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id)`,
	}},
	// v2: persistent user lifecycle state. Existing rows are 'ready' — they
	// were fully created under the old (no-state) schema.
	{2, []string{
		`ALTER TABLE users ADD COLUMN status TEXT NOT NULL DEFAULT 'ready'`,
	}},
	// v3: per-user init PID baseline for bandwidth sampling. A PID change is
	// the reliable "counters genuinely reset" signal (container restart /
	// reinstall); without it a late out-of-order sample would be mistaken for
	// a restart and double-counted (review P2-3).
	{3, []string{
		`ALTER TABLE bandwidth ADD COLUMN last_pid INTEGER NOT NULL DEFAULT 0`,
	}},
}

// userStatus values (kept as plain strings in the DB).
const (
	StatusReady        = "ready"
	StatusCreating     = "creating"
	StatusReinstalling = "reinstalling"
	StatusFailed       = "failed"
)

func (d *DB) migrate() error {
	// Version 1 baseline: the original code created these tables directly (no
	// migration tracking). Ensure they exist before checking the version table.
	for _, s := range migrations[0].stmts {
		if _, err := d.sql.Exec(s); err != nil {
			return fmt.Errorf("migrate v1: %w", err)
		}
	}
	applied, err := d.appliedMigrations()
	if err != nil {
		return err
	}
	cur := 1
	for v := range applied {
		if v > cur {
			cur = v
		}
	}
	if cur > schemaVersion {
		return fmt.Errorf("database schema version %d is newer than this binary (%d) — upgrade the panel before opening this database", cur, schemaVersion)
	}
	for _, m := range migrations {
		if m.version <= cur {
			continue
		}
		tx, err := d.sql.Begin()
		if err != nil {
			return fmt.Errorf("migrate v%d begin: %w", m.version, err)
		}
		for _, s := range m.stmts {
			if _, err := tx.Exec(s); err != nil {
				tx.Rollback()
				return fmt.Errorf("migrate v%d: %w", m.version, err)
			}
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES(?,?)`,
			m.version, now()); err != nil {
			tx.Rollback()
			return fmt.Errorf("migrate v%d record: %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migrate v%d commit: %w", m.version, err)
		}
	}
	return nil
}

// appliedMigrations returns the set of recorded migration versions.
func (d *DB) appliedMigrations() (map[int]bool, error) {
	rows, err := d.sql.Query(`SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()
	out := map[int]bool{}
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}


func now() string { return time.Now().UTC().Format(time.RFC3339) }
