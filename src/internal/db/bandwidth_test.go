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

func wantBandwidth(t *testing.T, tr *Bandwidth, period string, up, down uint64, rx, tx int64, pid int64) {
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
	if tr.LastPID != pid {
		t.Errorf("last_pid = %d, want %d", tr.LastPID, pid)
	}
}

func TestApplyBandwidth(t *testing.T) {
	d := openTestDB(t)
	u := mkUser(t, d, "alice", 1)

	// First sample: row is inserted, baselines become the current counters,
	// nothing is counted yet (pid is recorded but the first sample has no
	// baseline to compare against, so it acts as a reset: 0 is added).
	if err := d.ApplyBandwidth(u.ID, "2026-08", 1000, 500, 111); err != nil {
		t.Fatal(err)
	}
	tr, err := d.GetBandwidth(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantBandwidth(t, tr, "2026-08", 0, 0, 1000, 500, 111)

	// Normal growth in the same month, same pid.
	if err := d.ApplyBandwidth(u.ID, "2026-08", 1500, 700, 111); err != nil {
		t.Fatal(err)
	}
	tr, _ = d.GetBandwidth(u.ID)
	wantBandwidth(t, tr, "2026-08", 200, 500, 1500, 700, 111)

	// Counter reset (container restart): pid changed -> post-reset bandwidth counts.
	if err := d.ApplyBandwidth(u.ID, "2026-08", 100, 50, 222); err != nil {
		t.Fatal(err)
	}
	tr, _ = d.GetBandwidth(u.ID)
	wantBandwidth(t, tr, "2026-08", 250, 600, 100, 50, 222)

	// Month rollover: totals are zeroed, then the delta is applied.
	if err := d.ApplyBandwidth(u.ID, "2026-09", 250, 120, 222); err != nil {
		t.Fatal(err)
	}
	tr, _ = d.GetBandwidth(u.ID)
	wantBandwidth(t, tr, "2026-09", 70, 150, 250, 120, 222)
}

// TestApplyBandwidthOutOfOrder guards review P2-3: a late, LOWER counter from a
// concurrent sampler (panel daemon + CLI) with the SAME pid must be dropped —
// it is an old sample, not a container restart. Before the pid check this was
// treated as a restart and the post-reset value was added twice.
func TestApplyBandwidthOutOfOrder(t *testing.T) {
	d := openTestDB(t)
	u := mkUser(t, d, "carol", 3)

	if err := d.ApplyBandwidth(u.ID, "2026-08", 2000, 1000, 333); err != nil {
		t.Fatal(err)
	}
	// A newer sample advances the totals.
	if err := d.ApplyBandwidth(u.ID, "2026-08", 5000, 3000, 333); err != nil {
		t.Fatal(err)
	}
	tr, _ := d.GetBandwidth(u.ID)
	// down += 5000-2000 = 3000, up += 3000-1000 = 2000
	wantBandwidth(t, tr, "2026-08", 2000, 3000, 5000, 3000, 333)

	// A stale, lower sample with the SAME pid arrives late: it must NOT add
	// anything (dropped), only refresh the baseline.
	if err := d.ApplyBandwidth(u.ID, "2026-08", 2500, 1500, 333); err != nil {
		t.Fatal(err)
	}
	tr, _ = d.GetBandwidth(u.ID)
	wantBandwidth(t, tr, "2026-08", 2000, 3000, 2500, 1500, 333)
}

// TestApplyBandwidthResetNewPid covers a genuine container restart: pid changes,
// so the post-reset counters are counted even though they are lower than the
// pre-restart baseline.
func TestApplyBandwidthResetNewPid(t *testing.T) {
	d := openTestDB(t)
	u := mkUser(t, d, "dave", 4)

	if err := d.ApplyBandwidth(u.ID, "2026-08", 9000, 4000, 444); err != nil {
		t.Fatal(err)
	}
	// Container restarts: new pid, counters start near zero again.
	if err := d.ApplyBandwidth(u.ID, "2026-08", 300, 100, 555); err != nil {
		t.Fatal(err)
	}
	tr, _ := d.GetBandwidth(u.ID)
	// 300 + 100 counted (post-reset), baseline updated to the new pid's counters.
	wantBandwidth(t, tr, "2026-08", 100, 300, 300, 100, 555)
}

func TestBandwidthCascadeDelete(t *testing.T) {
	d := openTestDB(t)
	u := mkUser(t, d, "bob", 2)
	if err := d.ApplyBandwidth(u.ID, "2026-08", 10, 20, 0); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteUser(u.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetBandwidth(u.ID); err == nil {
		t.Fatal("bandwidth row should be deleted with the user")
	}
}
