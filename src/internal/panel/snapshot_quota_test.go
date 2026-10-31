package panel

import "testing"

func TestOverviewUsesIndependentSnapshotQuota(t *testing.T) {
	srv, d := newTestServer(t)
	for i, tc := range []struct {
		name  string
		ip    string
		limit int
	}{
		{"alice", "192.0.2.2", 2},
		{"bob", "192.0.2.3", 7},
	} {
		u, err := d.CreateUser(tc.name, "unused", tc.ip, i+1, 30001+i, 10000+i*100, 10, 64, 1)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.UpdateSnapshotLimit(u.ID, tc.limit); err != nil {
			t.Fatal(err)
		}
	}
	legacyLimit := 0
	srv.cfg.Snapshots.LegacyLimit = &legacyLimit
	for _, tc := range []struct {
		name  string
		limit int
	}{{"alice", 2}, {"bob", 7}} {
		u, err := d.GetUserByName(tc.name)
		if err != nil {
			t.Fatal(err)
		}
		data := srv.buildData(u, "", "")
		if data.SnapshotLimit != tc.limit || data.SnapshotFixedLimit != tc.limit {
			t.Fatalf("%s saw quota %d/%d, want=%d; legacy configuration leaked into overview", tc.name, data.SnapshotLimit, data.SnapshotFixedLimit, tc.limit)
		}
	}
}
