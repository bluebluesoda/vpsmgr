package db

import (
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
