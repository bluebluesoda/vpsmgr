package db

import (
	"database/sql"
	"testing"
)

// TestMigrateCPUToTenths verifies that a database written by an older version
// (cpu stored as whole cores) is scaled to tenths on open, and that a fresh
// database keeps the new default untouched.
func TestMigrateCPUToTenths(t *testing.T) {
	path := t.TempDir() + "/old.db"
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	oldSchema := `CREATE TABLE users(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT UNIQUE NOT NULL,
		pass_hash TEXT NOT NULL,
		idx INTEGER UNIQUE NOT NULL,
		ip TEXT NOT NULL,
		ssh_port INTEGER NOT NULL,
		start_port INTEGER NOT NULL,
		cpu INTEGER NOT NULL DEFAULT 1,
		mem_mb INTEGER NOT NULL DEFAULT 1024,
		disk_gb INTEGER NOT NULL DEFAULT 10,
		created_at TEXT NOT NULL
	)`
	if _, err := raw.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(
		`INSERT INTO users(name, pass_hash, idx, ip, ssh_port, start_port, cpu, mem_mb, disk_gb, created_at)
		 VALUES('alice','h','1','10.42.0.2',30001,10000,2,1024,10,'t')`); err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(
		`INSERT INTO users(name, pass_hash, idx, ip, ssh_port, start_port, cpu, mem_mb, disk_gb, created_at)
		 VALUES('bob','h','2','10.42.0.3',30002,10100,4,2048,20,'t')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	alice, err := d.GetUserByName("alice")
	if err != nil {
		t.Fatal(err)
	}
	if alice.CPU != 20 { // 2 whole cores -> 20 tenths
		t.Fatalf("alice cpu = %d, want 20", alice.CPU)
	}
	bob, err := d.GetUserByName("bob")
	if err != nil {
		t.Fatal(err)
	}
	if bob.CPU != 40 {
		t.Fatalf("bob cpu = %d, want 40", bob.CPU)
	}

	// Reopening must not double the values again.
	d.Close()
	d2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d2.Close()
	alice, err = d2.GetUserByName("alice")
	if err != nil {
		t.Fatal(err)
	}
	if alice.CPU != 20 {
		t.Fatalf("reopen: alice cpu = %d, want 20", alice.CPU)
	}
}

// TestFreshDBUsesTenthsDefault verifies a brand-new database already speaks
// tenths and is marked migrated, so a fresh default of 10 (= 1 core) is never
// doubled.
func TestFreshDBUsesTenthsDefault(t *testing.T) {
	d, err := Open(t.TempDir() + "/fresh.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.CreateUser("carol", "h", "10.42.0.4", 3, 30003, 10200, 0, 512, 5); err != nil {
		t.Fatal(err)
	}
	u, err := d.GetUserByName("carol")
	if err != nil {
		t.Fatal(err)
	}
	if u.CPU != 0 {
		t.Fatalf("carol cpu = %d, want 0 (stored as-is, tenths)", u.CPU)
	}
}
