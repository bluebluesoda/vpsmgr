package db

import (
	"database/sql"
	"fmt"
	"net/url"
	"time"

	_ "modernc.org/sqlite"
)

type DB struct {
	sql *sql.DB
}

func Open(path string) (*DB, error) {
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
	return d, nil
}

func (d *DB) Close() error { return d.sql.Close() }

func (d *DB) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS users(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			pass_hash TEXT NOT NULL,
			idx INTEGER UNIQUE NOT NULL,
			ip TEXT NOT NULL,
			ssh_port INTEGER UNIQUE NOT NULL,
			start_port INTEGER NOT NULL,
			init_script TEXT NOT NULL DEFAULT '',
			traffic_quota_gb INTEGER NOT NULL DEFAULT 0,
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
		`CREATE TABLE IF NOT EXISTS traffic(
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
		`CREATE INDEX IF NOT EXISTS idx_domains_user ON domains(user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id)`,
	}
	for _, s := range stmts {
		if _, err := d.sql.Exec(s); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	if err := d.migrateUserColumns(); err != nil {
		return fmt.Errorf("migrate: add user columns: %w", err)
	}
	if err := d.migrateDomainColumns(); err != nil {
		return fmt.Errorf("migrate: add domain columns: %w", err)
	}
	if err := d.migrateAuditTargetDrop(); err != nil {
		return fmt.Errorf("migrate: drop audit target: %w", err)
	}
	return d.migrateCPU()
}

// migrateAuditTargetDrop removes the audit_log.target column that existed in an
// early v0.3 dev schema. The audit design has no "target" concept — the actor
// encodes it ("000+<user>" for admin-on-user actions). SQLite >= 3.35 supports
// ALTER TABLE DROP COLUMN; the PRAGMA check keeps a fresh DB untouched.
func (d *DB) migrateAuditTargetDrop() error {
	rows, err := d.sql.Query(`PRAGMA table_info(audit_log)`)
	if err != nil {
		return err
	}
	hasTarget := false
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "target" {
			hasTarget = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if !hasTarget {
		return nil
	}
	_, err = d.sql.Exec(`ALTER TABLE audit_log DROP COLUMN target`)
	return err
}

// migrateDomainColumns adds columns to the domains table that were introduced
// after the original schema (proxy_protocol, updated_at). PRAGMA-checked, same
// approach as migrateUserColumns.
func (d *DB) migrateDomainColumns() error {
	want := map[string]string{
		"proxy_protocol": `ALTER TABLE domains ADD COLUMN proxy_protocol INTEGER NOT NULL DEFAULT 0`,
		"updated_at":     `ALTER TABLE domains ADD COLUMN updated_at TEXT NOT NULL DEFAULT ''`,
	}
	rows, err := d.sql.Query(`PRAGMA table_info(domains)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return err
		}
		delete(want, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, stmt := range want {
		if _, err := d.sql.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// migrateUserColumns adds columns to the users table that were introduced
// after the original schema (init_script, traffic_quota_gb). A fresh database
// already has them via CREATE TABLE; this only matters for a pre-existing DB
// that survived from an earlier dev build. The PRAGMA check is used instead of
// matching the driver's duplicate-column error string.
func (d *DB) migrateUserColumns() error {
	want := map[string]string{
		"init_script":      `ALTER TABLE users ADD COLUMN init_script TEXT NOT NULL DEFAULT ''`,
		"traffic_quota_gb": `ALTER TABLE users ADD COLUMN traffic_quota_gb INTEGER NOT NULL DEFAULT 0`,
	}
	rows, err := d.sql.Query(`PRAGMA table_info(users)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return err
		}
		delete(want, name)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, stmt := range want {
		if _, err := d.sql.Exec(stmt); err != nil {
			return err
		}
	}
	return nil
}

// migrateCPU converts the cpu column from whole cores to tenths of a core
// (cpu=1 now means 0.1 cores; cpu=10 means 1 core). Old databases stored whole
// cores as integers, so multiply by 10. Guarded by PRAGMA user_version so it
// runs exactly once — the fresh-install default (10) must never be doubled.
func (d *DB) migrateCPU() error {
	var v int
	if err := d.sql.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return fmt.Errorf("migrate: read user_version: %w", err)
	}
	if v >= 1 {
		return nil
	}
	// The UPDATE and the version bump must commit together: a crash between
	// them would re-run the UPDATE on the next start and double every cpu
	// value. PRAGMA user_version is a connection-local statement, so it runs
	// on the transaction's connection and is rolled back with it.
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("migrate: begin cpu scale: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE users SET cpu = cpu * 10`); err != nil {
		return fmt.Errorf("migrate: scale cpu to tenths: %w", err)
	}
	if _, err := tx.Exec(`PRAGMA user_version = 1`); err != nil {
		return fmt.Errorf("migrate: set user_version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("migrate: commit cpu scale: %w", err)
	}
	return nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }
