package db

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	_ "modernc.org/sqlite"
)

func TestBlockedDomainsRoundTrip(t *testing.T) {
	d, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// A fresh DB carries the seeded defaults (migration v5); clear them first
	// so the test starts from a known, empty list.
	if err := d.SetBlockedDomains(nil); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetBlockedDomains()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("after clear: got %v, want empty", got)
	}

	list := []string{"example.co.uk", "spam.example", "ads.io"}
	if err := d.SetBlockedDomains(list); err != nil {
		t.Fatal(err)
	}
	got, err = d.GetBlockedDomains()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, list) {
		t.Errorf("round trip = %v, want %v", got, list)
	}

	// Overwrite with fewer entries: the old ones must not linger.
	if err := d.SetBlockedDomains([]string{"only.com"}); err != nil {
		t.Fatal(err)
	}
	got, err = d.GetBlockedDomains()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "only.com" {
		t.Errorf("after overwrite = %v, want [only.com]", got)
	}

	// Empty list clears the value.
	if err := d.SetBlockedDomains(nil); err != nil {
		t.Fatal(err)
	}
	got, err = d.GetBlockedDomains()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("after clear = %v, want empty", got)
	}
}

// TestBlockedDomainsSeededOnOpen verifies migration v5 seeds the default list
// on a fresh database and that a second open does not duplicate or re-seed it.
func TestBlockedDomainsSeededOnOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seed.db")
	d1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d1.GetBlockedDomains()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, DefaultBlockedDomains) {
		t.Errorf("seeded list = %v, want defaults %v", got, DefaultBlockedDomains)
	}
	d1.Close()

	d2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	got, err = d2.GetBlockedDomains()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, DefaultBlockedDomains) {
		t.Errorf("after reopen = %v, want unchanged defaults", got)
	}
}

// TestBlockedDomainsSeedRespectsAdminEdits verifies an admin's edit (and a
// full clear) survives a restart — migration v5 never re-seeds an existing key.
func TestBlockedDomainsSeedRespectsAdminEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "edit.db")
	d1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := d1.SetBlockedDomains([]string{"only.me"}); err != nil {
		t.Fatal(err)
	}
	d1.Close()

	d2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	got, err := d2.GetBlockedDomains()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "only.me" {
		t.Errorf("after reopen = %v, want [only.me]", got)
	}
}

// TestBlockedDomainsSeedSkipsPreExistingKey simulates the real upgrade: a
// v4-era database that already carries a blocked_domains value must not have
// the defaults imposed over it.
func TestBlockedDomainsSeedSkipsPreExistingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v4.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{
		`CREATE TABLE settings(key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`,
		`INSERT INTO schema_migrations(version, applied_at) VALUES(1,'x'),(2,'x'),(3,'x'),(4,'x')`,
		`INSERT INTO settings(key, value) VALUES('` + SettingBlockedDomains + `', 'custom.only')`,
	} {
		if _, err := raw.Exec(s); err != nil {
			raw.Close()
			t.Fatalf("seed v4 db: %v", err)
		}
	}
	raw.Close()

	d, err := Open(path)
	if err != nil {
		t.Fatalf("open v4 db: %v", err)
	}
	defer d.Close()
	got, err := d.GetBlockedDomains()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "custom.only" {
		t.Errorf("pre-existing list = %v, want [custom.only] (defaults must not be imposed)", got)
	}
}
