package db

import (
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func mkUser(t *testing.T, d *DB, name string, idx int) *User {
	t.Helper()
	u, err := d.CreateUser(name, "h", "10.42.0.2", idx, 30000+idx, 10000, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func wantBandwidth(t *testing.T, tr *Bandwidth, period string, up, down uint64, rx, tx int64) {
	t.Helper()
	if tr.Period != period {
		t.Errorf("period = %q, want %q", tr.Period, period)
	}
	if tr.Upload != up {
		t.Errorf("upload = %d, want %d", tr.Upload, up)
	}
	if tr.Download != down {
		t.Errorf("download = %d, want %d", tr.Download, down)
	}
	if tr.LastRX != rx {
		t.Errorf("last_rx = %d, want %d", tr.LastRX, rx)
	}
	if tr.LastTX != tx {
		t.Errorf("last_tx = %d, want %d", tr.LastTX, tx)
	}
}

func TestApplyBandwidth(t *testing.T) {
	d := openTestDB(t)
	u := mkUser(t, d, "alice", 1)

	// First sample: row is inserted, baselines become the current counters,
	// nothing is counted yet.
	if err := d.ApplyBandwidth(u.ID, "2026-08", 1000, 500); err != nil {
		t.Fatal(err)
	}
	tr, err := d.GetBandwidth(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantBandwidth(t, tr, "2026-08", 0, 0, 1000, 500)

	// Normal growth in the same month.
	if err := d.ApplyBandwidth(u.ID, "2026-08", 1500, 700); err != nil {
		t.Fatal(err)
	}
	tr, _ = d.GetBandwidth(u.ID)
	wantBandwidth(t, tr, "2026-08", 200, 500, 1500, 700)

	// Counter reset (container restart): only the post-reset bandwidth counts.
	if err := d.ApplyBandwidth(u.ID, "2026-08", 100, 50); err != nil {
		t.Fatal(err)
	}
	tr, _ = d.GetBandwidth(u.ID)
	wantBandwidth(t, tr, "2026-08", 250, 600, 100, 50)

	// Month rollover: totals are zeroed, then the delta is applied.
	if err := d.ApplyBandwidth(u.ID, "2026-09", 250, 120); err != nil {
		t.Fatal(err)
	}
	tr, _ = d.GetBandwidth(u.ID)
	wantBandwidth(t, tr, "2026-09", 70, 150, 250, 120)
}

func TestBandwidthCascadeDelete(t *testing.T) {
	d := openTestDB(t)
	u := mkUser(t, d, "bob", 2)
	if err := d.ApplyBandwidth(u.ID, "2026-08", 10, 20); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteUser(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetBandwidth(u.ID); err == nil {
		t.Fatal("bandwidth row should be deleted with the user")
	}
}
