package db

import (
	"database/sql"
	"testing"
)

// TestInitScriptRoundTrip verifies the init_script column stores and reads
// back a user's custom script.
func TestInitScriptRoundTrip(t *testing.T) {
	d, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	u, err := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	if u.InitScript != "" {
		t.Fatalf("new user init_script = %q, want empty", u.InitScript)
	}
	script := "#!/bin/bash\napt-get update && apt-get install -y nginx"
	if err := d.UpdateInitScript(u.ID, script); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetUserByName("alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.InitScript != script {
		t.Errorf("init_script = %q, want %q", got.InitScript, script)
	}
	if err := d.UpdateInitScript(u.ID, ""); err != nil {
		t.Fatal(err)
	}
	got, err = d.GetUserByName("alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.InitScript != "" {
		t.Errorf("init_script not cleared: %q", got.InitScript)
	}
}

// TestMigrateAddsInitScriptColumn verifies an existing database created before
// the init_script column gets it added by the migration on open.
func TestMigrateAddsInitScriptColumn(t *testing.T) {
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
		cpu INTEGER NOT NULL DEFAULT 10,
		mem_mb INTEGER NOT NULL DEFAULT 1024,
		disk_gb INTEGER NOT NULL DEFAULT 10,
		created_at TEXT NOT NULL
	)`
	if _, err := raw.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	u, err := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	if u.InitScript != "" {
		t.Errorf("migrated user init_script = %q, want empty default", u.InitScript)
	}
	if u.TrafficQuotaGB != 0 {
		t.Errorf("migrated user traffic_quota_gb = %d, want 0", u.TrafficQuotaGB)
	}
	// A SELECT must be able to read both columns (proves the ALTERs ran).
	got, err := d.GetUserByName("alice")
	if err != nil {
		t.Fatalf("GetUserByName after migration: %v", err)
	}
	if got.InitScript != "" || got.TrafficQuotaGB != 0 {
		t.Errorf("read-back after migration = %q/%d", got.InitScript, got.TrafficQuotaGB)
	}
}

// TestTrafficQuotaRoundTrip verifies the traffic_quota_gb column stores and
// reads back a user's monthly quota (0 = unlimited).
func TestTrafficQuotaRoundTrip(t *testing.T) {
	d, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	u, err := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	if u.TrafficQuotaGB != 0 {
		t.Fatalf("new user traffic_quota_gb = %d, want 0", u.TrafficQuotaGB)
	}
	if err := d.UpdateTrafficQuota(u.ID, 100); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetUserByName("alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.TrafficQuotaGB != 100 {
		t.Errorf("traffic_quota_gb = %d, want 100", got.TrafficQuotaGB)
	}
	if err := d.UpdateTrafficQuota(u.ID, 0); err != nil {
		t.Fatal(err)
	}
	got, _ = d.GetUserByName("alice")
	if got.TrafficQuotaGB != 0 {
		t.Errorf("traffic_quota_gb not reset: %d", got.TrafficQuotaGB)
	}
}
