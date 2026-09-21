package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
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
const schemaVersion = 20

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
	// v4: per-user pool-mode IPv6 address. NULL = no IPv6 assigned (prefix
	// mode derives addresses on the fly; pool mode stores the assignment).
	// The UNIQUE index is the concurrency backstop: one address can never be
	// handed to two users. The address is released simply by deleting the
	// user row (the column dies with it).
	{4, []string{
		`ALTER TABLE users ADD COLUMN ipv6_address TEXT`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_ipv6 ON users(ipv6_address)`,
	}},
	// v5: seed the default admin blocked-domains list. The INSERT...SELECT
	// only fires when the settings key was never written, so an admin's later
	// edits (or a full clear) are never re-seeded on restart.
	{5, []string{
		`INSERT INTO settings(key, value)
		 SELECT '` + SettingBlockedDomains + `', '` + sqliteStr(strings.Join(DefaultBlockedDomains, "\n")) + `'
		 WHERE NOT EXISTS (SELECT 1 FROM settings WHERE key = '` + SettingBlockedDomains + `')`,
	}},
	// v6: minute resource history. Values are deliberately compact: memory and
	// filesystem usage use MiB, CPU uses tenths of a percent, and counters remain
	// bytes so bandwidth graphs can be calculated without losing small traffic.
	{6, []string{
		`ALTER TABLE bandwidth ADD COLUMN last_boot_time INTEGER NOT NULL DEFAULT 0`,
		`CREATE TABLE resource_samples(
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			sample_minute INTEGER NOT NULL,
			state INTEGER NOT NULL,
			boot_time INTEGER,
			cpu_seconds_ns INTEGER,
			cpu_pct_x10 INTEGER NOT NULL DEFAULT -1,
			memory_mib INTEGER,
			processes INTEGER,
			disk_used_mib INTEGER,
			rx_bytes_total INTEGER,
			tx_bytes_total INTEGER,
			disk_read_bytes_total INTEGER,
			disk_write_bytes_total INTEGER,
			PRIMARY KEY(user_id, sample_minute)
		) WITHOUT ROWID`,
		`CREATE INDEX resource_samples_time ON resource_samples(sample_minute)`,
	}},
	// v7: per-user SSH public keys managed from the panel. Keys are stored
	// clean (type + base64 body, comment stripped); `active` decides which are
	// written into ~/.ssh/authorized_keys. Cascades away with the user.
	{7, []string{
		`CREATE TABLE IF NOT EXISTS ssh_keys(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			name TEXT NOT NULL,
			key TEXT NOT NULL,
			active INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_ssh_keys_user ON ssh_keys(user_id)`,
	}},
	// v8: admin-side public keys. Same shape as ssh_keys but not tied to a
	// user — the operator's own key store (not injected into any container yet;
	// the plumbing is a future feature). Kept in its own table so the per-user
	// flow and its queries stay untouched.
	{8, []string{
		`CREATE TABLE IF NOT EXISTS admin_keys(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			key TEXT NOT NULL,
			active INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL
		)`,
	}},
	// v9: per-user sticky notes. One opaque row per user holding the whole
	// encrypted envelope as a single TEXT blob (the panel encrypts client-side;
	// the server never sees plaintext). Empty string = never enabled / reset.
	{9, []string{
		`CREATE TABLE IF NOT EXISTS sticky_notes(
			user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
			data TEXT NOT NULL DEFAULT ''
		)`,
	}},
	// v10: per-user grants of the operator's admin keys. A row means "this
	// user has activated this admin key" — its content is then written into the
	// user's authorized_keys (admin SSH access to that machine). Kept separate
	// from ssh_keys so the user's own list and the admin's store stay
	// independent; UNIQUE(user_id, admin_key_id) makes a grant one-per-pair and
	// both FKs cascade so a deleted user or admin key drops its grants.
	{10, []string{
		`CREATE TABLE IF NOT EXISTS admin_key_grants(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
			admin_key_id INTEGER NOT NULL REFERENCES admin_keys(id) ON DELETE CASCADE,
			created_at TEXT NOT NULL,
			UNIQUE(user_id, admin_key_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_admin_key_grants_user ON admin_key_grants(user_id)`,
	}},
	// v11: mark user sessions created by the operator ("log in as user"). Lets
	// the user panel tell an admin impersonation apart from the user's own
	// login, so audit events can be attributed "000+<user>". Existing rows are
	// normal (0).
	{11, []string{
		`ALTER TABLE sessions ADD COLUMN impersonated INTEGER NOT NULL DEFAULT 0`,
	}},
	// v12: per-user accent color assigned by the operator (empty = default).
	// Stored as a hex string from the admin panel's fixed palette; the user
	// panel tints its background/accents with it so an impersonating operator
	// can tell users apart at a glance. Only the admin can set it.
	{12, []string{
		`ALTER TABLE users ADD COLUMN color TEXT NOT NULL DEFAULT ''`,
	}},
	// v13: port blocks are no longer derived from idx (net.user_ports
	// decoupled them), so start_port uniqueness must be enforced directly — a
	// cross-process race (CLI + panel daemon) could otherwise hand the same
	// block to two users. Existing rows are already unique (they came from the
	// old idx derivation), so the index applies cleanly.
	{13, []string{
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_start_port ON users(start_port)`,
	}},
	// v14: snapshot shares. A user may publish one checkpoint behind a random
	// code; anyone can install a new container by cloning that snapshot. One
	// active share per container (UNIQUE(user_id)) — publishing a new one
	// replaces the old. Cascades away with the user.
	{14, []string{
		`CREATE TABLE IF NOT EXISTS snapshot_shares(
			code TEXT PRIMARY KEY,
			user_id INTEGER NOT NULL UNIQUE REFERENCES users(id) ON DELETE CASCADE,
			snapshot TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
	}},
	// v15: knowledge-base articles, authored by the operator in the admin panel
	// and shown read-only to users. Content is Markdown, rendered server-side.
	{15, []string{
		`CREATE TABLE IF NOT EXISTS knowledge(
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			title TEXT NOT NULL,
			content TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
	}},
	// v16: optional quota validity deadline. RFC3339 UTC (same format as
	// created_at); '' = permanent (no expiry). When the deadline passes the
	// container is force-stopped and the account is locked to read-only until
	// an admin extends it.
	{16, []string{
		`ALTER TABLE users ADD COLUMN expires_at TEXT NOT NULL DEFAULT ''`,
	}},
	// v17: admin panel sessions move out of process memory so a panel restart
	// (upgrade, crash, `systemctl restart vps`) no longer logs the operator out.
	// Only the SHA-256 of the token is stored, same as user sessions. The rows
	// carry no user reference — the admin panel authenticates with a password
	// only — so this is a separate table from `sessions`.
	{17, []string{
		`CREATE TABLE IF NOT EXISTS admin_sessions(
			token TEXT PRIMARY KEY,
			expires_at INTEGER NOT NULL
		)`,
	}},
	// v18: the domain-proxy runtime mirror moves from the legacy "traefik" key
	// to "haproxy" (the config field net.traefik was renamed to net.haproxy
	// when vpsmgr replaced Traefik with HAProxy). The stored value is a plain
	// "true"/"false" string in both spellings, so it is carried over verbatim.
	//
	// Both statements are unconditional and idempotent, which is what makes
	// this safe for every upgrade path:
	//   - installing over a Traefik-era DB: only "traefik" exists, the INSERT
	//     copies it to "haproxy", the DELETE drops the old row.
	//   - a DB that already carries "haproxy" (fresh install, or a re-run
	//     after a failure): the INSERT hits ON CONFLICT and keeps the NEW
	//     value, the DELETE still cleans up a stale "traefik" row if present.
	//   - a DB with neither row: both statements are no-ops.
	// Migration v18 runs exactly once (recorded in schema_migrations), which
	// is why the "keep the new value" rule is expressed in SQL rather than in
	// Go — there is no second chance to get it wrong.
	{18, []string{
		`INSERT INTO settings(key, value)
			SELECT 'haproxy', value FROM settings WHERE key = 'traefik'
			ON CONFLICT(key) DO NOTHING`,
		`DELETE FROM settings WHERE key = 'traefik'`,
	}},
	// v19: a container's prefix-mode /112 is no longer derived from its
	// username but stored, and new accounts get a random one.
	//
	// Deriving it made the suffix of an address a global constant for a given
	// name — sha256("alice") is the same 32 bits on every host running this
	// panel — so an address identified its owner to anyone who knew the scheme
	// and could enumerate names. That is exactly what the random container
	// hostname exists to prevent.
	//
	// Accounts that already exist are seeded with the value they have been
	// using, so no address changes and no container has to be touched; only
	// accounts created from now on are random. NULL means "no index", which is
	// every pool-mode account (it stores a whole /128 instead) and is why the
	// unique index tolerates many of them.
	//
	// The seeded set cannot collide: the old code refused to create a name
	// whose block collided with an existing one, or whose block would have
	// contained the bridge gateway, so what is being written here was already
	// unique.
	{19, []string{
		`ALTER TABLE users ADD COLUMN ipv6_index INTEGER`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_ipv6_index ON users(ipv6_index)`,
	}},
	// v20 adds the optional whole /64 block a container can be given from
	// net.ipv6_extra_prefix (an extra prefix routed alongside the /112 primary
	// address). NULL means "this account has none" — the state of every account
	// on an install that never set the extra prefix, which is why the unique
	// index has to tolerate many NULLs. The CIDR is stored as given rather than
	// re-derived from the config, so changing the prefix later never invalidates
	// an existing assignment.
	{20, []string{
		`ALTER TABLE users ADD COLUMN ipv6_extra_block TEXT`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_users_ipv6_extra_block ON users(ipv6_extra_block)`,
	}},
}

// migrationSteps are the Go-side halves of a migration whose data cannot be
// expressed in SQL: v19 seeds its new column by hashing each username. Each
// runs inside the migration's own transaction, before the version is recorded,
// so a failure leaves the version unrecorded and the step is retried.
var migrationSteps = map[int]func(*sql.Tx) error{
	19: seedIPv6Indexes,
}

// seedIPv6Indexes is v19's data step: fill the new column for every account
// that predates it with the block index its address has been derived from.
func seedIPv6Indexes(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT id, name FROM users WHERE ipv6_index IS NULL`)
	if err != nil {
		return err
	}
	type pending struct {
		id   int64
		name string
	}
	var todo []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.name); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range todo {
		if _, err := tx.Exec(`UPDATE users SET ipv6_index=? WHERE id=?`, legacyIPv6Index(p.name), p.id); err != nil {
			return err
		}
	}
	return nil
}

// legacyIPv6Index is the 32-bit block index the pre-v19 code derived from a
// username: the first four bytes of its sha256. Nothing derives an index any
// more — this exists only to seed accounts that predate the column, so that
// their addresses do not move.
func legacyIPv6Index(name string) int64 {
	h := sha256.Sum256([]byte(name))
	return int64(binary.BigEndian.Uint32(h[:4]))
}

// sqliteStr quotes a string literal for SQLite by doubling any single quote.
func sqliteStr(s string) string { return strings.ReplaceAll(s, "'", "''") }

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
	// Applied versions must form the run 2..cur (v1 is the baseline, created
	// unconditionally above and never recorded). A gap means rows went missing
	// from schema_migrations — the schema is then missing a step, and running
	// the later migrations anyway buries the problem in obscure "no such
	// column" failures much later. Refuse instead.
	for v := 2; v <= cur; v++ {
		if !applied[v] {
			return fmt.Errorf("database schema is missing migration v%d (recorded: %v) — schema_migrations is incomplete; restore a backup or rebuild the database", v, sortedVersions(applied))
		}
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
		if step := migrationSteps[m.version]; step != nil {
			if err := step(tx); err != nil {
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

// sortedVersions formats a set of applied migration versions for an error
// message, oldest first.
func sortedVersions(applied map[int]bool) []int {
	out := make([]int, 0, len(applied))
	for v := range applied {
		out = append(out, v)
	}
	sort.Ints(out)
	return out
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
