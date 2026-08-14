package db

import (
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
