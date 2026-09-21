package db

import (
	"path/filepath"
	"testing"
)

// rewindToV17 opens a fresh database, removes every v18+ migration marker,
// undoes the structural change those later migrations made, and clears the two
// settings keys this migration deals with. It returns the path so the caller
// can seed the v17-era rows and then re-open (which applies v18 onwards).
func rewindToV17(t *testing.T, rows map[string]string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pre-v18.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`DELETE FROM schema_migrations WHERE version >= 18`); err != nil {
		t.Fatal(err)
	}
	// Un-apply v19, which added users.ipv6_index. Unlike v18's statements an
	// ALTER is not idempotent, so a marker-only rewind would fail on the second
	// run with "duplicate column name". A migration added after this helper must
	// be undone here too.
	if _, err := d.sql.Exec(`DROP INDEX IF EXISTS idx_users_ipv6_index`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`ALTER TABLE users DROP COLUMN ipv6_index`); err != nil {
		t.Fatal(err)
	}
	// Un-apply v20, which added users.ipv6_extra_block (same ALTER caveat).
	if _, err := d.sql.Exec(`DROP INDEX IF EXISTS idx_users_ipv6_extra_block`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`ALTER TABLE users DROP COLUMN ipv6_extra_block`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`DELETE FROM settings WHERE key IN ('traefik','haproxy')`); err != nil {
		t.Fatal(err)
	}
	for k, v := range rows {
		if _, err := d.sql.Exec(`INSERT INTO settings(key, value) VALUES(?, ?)`, k, v); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()
	return path
}

// TestMigrateV18RenamesTraefikSetting: a Traefik-era DB carries the
// domain-proxy mirror under "traefik". Migration v18 must carry the value over
// to "haproxy" verbatim and delete the old row.
func TestMigrateV18RenamesTraefikSetting(t *testing.T) {
	path := rewindToV17(t, map[string]string{"traefik": "false"})

	d, err := Open(path) // applies v18
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	v, ok, err := d.GetSetting(SettingHaproxy)
	if err != nil || !ok {
		t.Fatalf("haproxy setting missing after migration (ok=%v err=%v)", ok, err)
	}
	if v != "false" {
		t.Errorf("haproxy = %q, want the legacy value \"false\"", v)
	}
	if _, ok, _ := d.GetSetting("traefik"); ok {
		t.Error("legacy traefik row survived the migration")
	}
}

// TestMigrateV18KeepsExistingHaproxyValue: when the new key already exists
// (fresh install, or a re-run after a partial migration), the migration must
// not clobber it with a stale legacy value — but it must still drop the old row.
func TestMigrateV18KeepsExistingHaproxyValue(t *testing.T) {
	path := rewindToV17(t, map[string]string{"traefik": "false", "haproxy": "true"})

	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	v, ok, err := d.GetSetting(SettingHaproxy)
	if err != nil || !ok {
		t.Fatalf("haproxy setting missing (ok=%v err=%v)", ok, err)
	}
	if v != "true" {
		t.Errorf("haproxy = %q, want the NEW value \"true\" to win", v)
	}
	if _, ok, _ := d.GetSetting("traefik"); ok {
		t.Error("legacy traefik row survived the migration")
	}
}

// TestMigrateV18NoRows: a DB with neither key must stay that way — the
// migration must not invent a row (an absent key means "fall back to the
// config file", which is a meaningful state).
func TestMigrateV18NoRows(t *testing.T) {
	path := rewindToV17(t, nil)

	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, ok, _ := d.GetSetting(SettingHaproxy); ok {
		t.Error("migration created a haproxy row out of nothing")
	}
}

// TestMigrateV18IsIdempotent: re-opening an already migrated DB must be a no-op
// (the migration is recorded in schema_migrations and never re-runs).
func TestMigrateV18IsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v18.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetSetting(SettingHaproxy, "true"); err != nil {
		t.Fatal(err)
	}
	d.Close()

	for i := 0; i < 3; i++ {
		d, err := Open(path)
		if err != nil {
			t.Fatalf("reopen %d: %v", i, err)
		}
		v, ok, err := d.GetSetting(SettingHaproxy)
		if err != nil || !ok || v != "true" {
			t.Fatalf("reopen %d: haproxy=(%q, %v, %v)", i, v, ok, err)
		}
		d.Close()
	}
}
