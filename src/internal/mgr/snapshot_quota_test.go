package mgr

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"vpsmgr/internal/lx"
)

func TestSnapshotQuotaReduction(t *testing.T) {
	for _, tc := range []struct {
		name        string
		fixed, auto int
		enabled     bool
		limit       int
		allowed     bool
	}{
		{"below existing", 1, 3, true, 3, false},
		{"equal existing", 1, 3, true, 4, true},
		{"reserved before first automatic", 4, 0, true, 4, false},
		{"one automatic position", 2, 1, true, 3, true},
		{"empty enabled to zero", 0, 0, true, 0, false},
		{"empty enabled to one", 0, 0, true, 1, true},
		{"empty disabled to zero", 0, 0, false, 0, true},
		{"fixed equal disabled", 3, 0, false, 3, true},
		{"fixed below disabled", 3, 0, false, 2, false},
		{"negative", 0, 0, false, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, u, f := newSnapshotManager(t, 5)
			seedCheckpoints(t, m, u, f, tc.fixed, tc.auto)
			if !tc.enabled {
				if err := m.SetSnapshotSchedule(u.Name, false, 0); err != nil {
					t.Fatal(err)
				}
			}
			before := append(f.snaps[:0:0], f.snaps...)
			policy, _ := m.SnapshotSchedule(u.Name)
			err := m.UpdateSnapshotLimit(u.Name, tc.limit)
			if (err == nil) != tc.allowed {
				t.Fatalf("quota=%d allowed=%v error=%v", tc.limit, tc.allowed, err)
			}
			fresh, err := m.db.GetUserByName(u.Name)
			if err != nil {
				t.Fatal(err)
			}
			want := 5
			if tc.allowed {
				want = tc.limit
			}
			if fresh.SnapshotLimit != want {
				t.Fatalf("persisted quota=%d, want=%d", fresh.SnapshotLimit, want)
			}
			afterPolicy, _ := m.SnapshotSchedule(u.Name)
			if !reflect.DeepEqual(policy, afterPolicy) || !reflect.DeepEqual(before, f.snaps) || len(f.calls) != 0 {
				t.Fatal("quota edit changed checkpoints, policy or deadline")
			}
		})
	}
}

func TestSnapshotQuotaCatalogFailure(t *testing.T) {
	m, u, f := newSnapshotManager(t, 5)
	f.listFail = true
	if err := m.UpdateSnapshotLimit(u.Name, 4); err == nil {
		t.Fatal("lowered quota without authoritative catalog")
	}
	if err := m.UpdateSnapshotLimit(u.Name, 6); err != nil {
		t.Fatalf("safe increase requires Incus: %v", err)
	}
	if err := m.UpdateSnapshotLimit(u.Name, 6); err != nil {
		t.Fatalf("unchanged quota requires Incus: %v", err)
	}
	fresh, _ := m.db.GetUserByName(u.Name)
	if fresh.SnapshotLimit != 6 {
		t.Fatal("increase was not persisted")
	}
}

func TestCombinedSnapshotQuotaWithoutIncus(t *testing.T) {
	m, u, _ := newSnapshotManager(t, 5)
	m.lx = lx.New("/nonexistent/vpsmgr-snapshot-quota-test.sock", 0)
	// CLI/admin forms carry the current resource values even when editing only
	// snapshots. Unchanged resources must not turn safe metadata edits into
	// Incus writes, while a reduction still requires the authoritative catalog.
	for _, limit := range []int{6, 6} {
		res, err := m.UpdateQuotasAndBandwidth(u.Name, u.CPU, u.MemMB, u.DiskGB, u.BandwidthQuotaGB, limit)
		if err != nil {
			t.Fatalf("safe combined quota=%d requires Incus: %v", limit, err)
		}
		if res.User.SnapshotLimit != limit {
			t.Fatal("returned stale snapshot quota")
		}
	}
	if _, err := m.UpdateQuotasAndBandwidth(u.Name, u.CPU, u.MemMB, u.DiskGB, u.BandwidthQuotaGB, 5); err == nil {
		t.Fatal("combined reduction succeeded without Incus catalog")
	}
	fresh, err := m.db.GetUserByName(u.Name)
	if err != nil || fresh.SnapshotLimit != 6 || fresh.CPU != u.CPU || fresh.MemMB != u.MemMB || fresh.DiskGB != u.DiskGB {
		t.Fatalf("unexpected combined quota persistence: %+v, %v", fresh, err)
	}
}

func TestSnapshotLegacyQuotaDoesNotChangeExistingAccount(t *testing.T) {
	m, u, f := newSnapshotManager(t, 5)
	seedCheckpoints(t, m, u, f, 1, 3)
	m.cfg.Snapshots.LegacyLimit = new(int)
	if err := m.SnapshotCreate(u.Name); err != nil {
		t.Fatalf("legacy migration quota disabled existing account's manual creation: %v", err)
	}
	if err := runSnapshotDue(t, m, u); err != nil {
		t.Fatalf("legacy migration quota disabled existing account's automatic task: %v", err)
	}
	snaps, err := m.SnapshotList(u.Name)
	if err != nil || len(snaps) != 5 || fixedSnapshotCount(snaps) != 2 {
		t.Fatalf("wrong existing-account quota: %v, %v", snaps, err)
	}
	restarted := New(m.cfg, m.db)
	if err := runSnapshotDue(t, restarted, u); err != nil {
		t.Fatalf("restart reapplied legacy migration quota to existing account: %v", err)
	}
	fresh, _ := m.db.GetUserByName(u.Name)
	if fresh.SnapshotLimit != 5 {
		t.Fatal("default change rewrote stored quota")
	}
}

func TestCombinedQuotaEditValidatesAndRollsBack(t *testing.T) {
	m, u, f := newSnapshotManager(t, 5)
	seedCheckpoints(t, m, u, f, 4, 0)
	if _, err := m.UpdateQuotasAndBandwidth(u.Name, 20, 128, 2, 100, 4); err == nil {
		t.Fatal("combined edit consumed reserved automatic position")
	}
	fresh, _ := m.db.GetUserByName(u.Name)
	if fresh.SnapshotLimit != 5 || fresh.BandwidthQuotaGB != 0 || fresh.CPU != u.CPU || len(f.calls) != 0 {
		t.Fatal("rejected snapshot quota partially changed other resources")
	}
	if _, err := m.UpdateQuotasAndBandwidth(u.Name, 11, 0, 0, 100, 6); err == nil {
		t.Fatal("invalid CPU quota accepted")
	}
	fresh, _ = m.db.GetUserByName(u.Name)
	if fresh.SnapshotLimit != 5 || fresh.BandwidthQuotaGB != 0 {
		t.Fatal("failed resource edit did not roll back DB-only quotas")
	}
	if _, err := m.UpdateQuotasAndBandwidth(u.Name, 0, 0, 0, -1, 6); err != nil {
		t.Fatal(err)
	}
	fresh, _ = m.db.GetUserByName(u.Name)
	if fresh.SnapshotLimit != 6 {
		t.Fatal("valid combined snapshot quota was not persisted")
	}
}

func TestSnapshotQuotaWaitsForCreationAcrossManagers(t *testing.T) {
	m, u, f := newSnapshotManager(t, 5)
	seedCheckpoints(t, m, u, f, 1, 2)
	entered := make(chan struct{})
	f.entered, f.release = entered, make(chan struct{})
	other := New(m.cfg, m.db)
	runDone := make(chan error, 1)
	go func() { runDone <- runSnapshotDue(t, m, u) }()
	<-entered
	quotaDone := make(chan error, 1)
	go func() { quotaDone <- other.UpdateSnapshotLimit(u.Name, 3) }()
	select {
	case err := <-quotaDone:
		close(f.release)
		t.Fatalf("quota edit crossed in-flight creation: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(f.release)
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if err := <-quotaDone; err == nil {
		t.Fatal("quota edit ignored the newly created checkpoint")
	}
	fresh, _ := m.db.GetUserByName(u.Name)
	if fresh.SnapshotLimit != 5 || len(f.snaps) != 4 {
		t.Fatal("concurrent edit changed quota or deleted recovery points")
	}
}

func TestSnapshotQuotaNegativeCreation(t *testing.T) {
	m, _, f := newSnapshotManager(t, 5)
	negative := -1
	if _, err := m.Add("bob", AddOptions{CPU: 10, MemMB: 64, DiskGB: 1, SnapshotLimit: &negative}); err == nil || !strings.Contains(err.Error(), "snapshot quota") {
		t.Fatalf("negative creation quota=%v", err)
	}
	if len(f.calls) != 0 {
		t.Fatal("invalid creation performed Incus mutations")
	}
}

func TestTransferSnapshotQuotaCompatibility(t *testing.T) {
	for _, tc := range []struct {
		body string
		want *int
	}{
		{`{"user":"alice","cpu":10,"mem_mb":64,"disk_gb":1}`, nil},
		{`{"user":"alice","cpu":10,"mem_mb":64,"disk_gb":1,"snapshot_limit":0}`, new(int)},
		{`{"user":"alice","cpu":10,"mem_mb":64,"disk_gb":1,"snapshot_limit":7}`, func() *int { n := 7; return &n }()},
	} {
		opt, _, err := TransferMetaAccount([]byte(tc.body))
		if err != nil || !reflect.DeepEqual(opt.SnapshotLimit, tc.want) {
			t.Fatalf("import snapshot quota=%v, want=%v, err=%v", opt.SnapshotLimit, tc.want, err)
		}
	}
	m, u, _ := newSnapshotManager(t, 0)
	body, err := m.buildTransferMeta(u)
	if err != nil {
		t.Fatal(err)
	}
	var meta TransferMeta
	if err := json.Unmarshal(body, &meta); err != nil {
		t.Fatal(err)
	}
	if meta.SnapshotLimit == nil || *meta.SnapshotLimit != 0 {
		t.Fatal("export lost explicit zero quota")
	}
}
