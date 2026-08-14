package db

import (
	"database/sql"
	"testing"
)

func TestAuditLogRoundTripAndOrder(t *testing.T) {
	d, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.AddAuditLog("alice", "power.restart"); err != nil {
		t.Fatal(err)
	}
	if err := d.AddAuditLog("000+alice", "power.stop"); err != nil {
		t.Fatal(err)
	}
	rows, err := d.ListAuditLog(0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	// Newest first.
	if rows[0].Actor != "000+alice" || rows[0].Action != "power.stop" {
		t.Errorf("first row = %+v", rows[0])
	}
	if rows[1].Actor != "alice" || rows[1].Action != "power.restart" {
		t.Errorf("second row = %+v", rows[1])
	}
	// Offset pagination.
	page2, err := d.ListAuditLog(1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 || page2[0].Actor != "alice" {
		t.Errorf("page2 = %+v", page2)
	}
	n, err := d.AuditCount()
	if err != nil || n != 2 {
		t.Errorf("AuditCount = %d, %v; want 2", n, err)
	}
}

func TestAuditLogPrunesToRetention(t *testing.T) {
	d, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// Insert more than the cap; every insert prunes to the newest 5000.
	for i := 0; i < AuditRetention+50; i++ {
		if err := d.AddAuditLog("alice", "power.start"); err != nil {
			t.Fatal(err)
		}
	}
	n, err := d.AuditCount()
	if err != nil {
		t.Fatal(err)
	}
	if n != AuditRetention {
		t.Errorf("AuditCount = %d, want retention %d", n, AuditRetention)
	}
}

// TestAuditMigrateDropsTarget verifies that a DB created by an early v0.3 dev
// build (audit_log with a target column) is migrated: the column is dropped and
// AddAuditLog works against the new schema.
func TestAuditMigrateDropsTarget(t *testing.T) {
	path := t.TempDir() + "/old.db"
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE audit_log(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		actor TEXT NOT NULL,
		action TEXT NOT NULL,
		target TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL)`,
		`INSERT INTO audit_log(actor, action, target, created_at) VALUES('alice','power.start','','2026-01-01T00:00:00.000Z')`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	var cols []string
	rows, err := d.sql.Query(`PRAGMA table_info(audit_log)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, name)
	}
	for _, want := range []string{"id", "actor", "action", "created_at"} {
		found := false
		for _, c := range cols {
			if c == want {
				found = true
			}
		}
		if !found {
			t.Errorf("audit_log missing column %q after migration (have %v)", want, cols)
		}
	}
	for _, c := range cols {
		if c == "target" {
			t.Errorf("audit_log still has target column after migration")
		}
	}
	if err := d.AddAuditLog("000+alice", "power.stop"); err != nil {
		t.Fatalf("AddAuditLog against migrated schema: %v", err)
	}
}
