package mgr

import (
	"path/filepath"
	"testing"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
)

func TestSetInitScriptBounds(t *testing.T) {
	c := cfg.Default()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.CreateUser("alice", "x", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	m := New(c, d)
	big := make([]byte, cfg.MaxInitScriptBytes+1)
	for i := range big {
		big[i] = 'x'
	}
	if err := m.SetInitScript("alice", string(big)); err == nil {
		t.Fatal("oversize init script should be rejected")
	}
	if err := m.SetInitScript("alice", "#!/bin/bash\necho hi"); err != nil {
		t.Fatalf("normal init script should be accepted: %v", err)
	}
	u, err := d.GetUserByName("alice")
	if err != nil {
		t.Fatal(err)
	}
	if u.InitScript != "#!/bin/bash\necho hi" {
		t.Errorf("stored init script = %q", u.InitScript)
	}
	if err := m.SetInitScript("alice", ""); err != nil {
		t.Fatalf("clearing init script: %v", err)
	}
	u, err = d.GetUserByName("alice")
	if err != nil {
		t.Fatal(err)
	}
	if u.InitScript != "" {
		t.Errorf("init script not cleared: %q", u.InitScript)
	}
}
