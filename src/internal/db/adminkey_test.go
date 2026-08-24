package db

import (
	"path/filepath"
	"testing"
)

func TestAdminKeyCRUD(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	k1, err := d.AddAdminKey("ops", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBODYadminkeybogusX", true)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := d.AddAdminKey("ci", "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQYadminkey", false)
	if err != nil {
		t.Fatal(err)
	}
	if k1.ID == 0 || k2.ID == 0 {
		t.Fatal("expected autoincrement ids")
	}

	keys, err := d.ListAdminKeys()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("ListAdminKeys = %d keys, want 2", len(keys))
	}
	if !keys[0].Active || keys[1].Active {
		t.Errorf("active flags wrong: %+v %+v", keys[0].Active, keys[1].Active)
	}

	if err := d.UpdateAdminKey(k1.ID, "ops-laptop", k1.Key, false); err != nil {
		t.Fatal(err)
	}
	keys, _ = d.ListAdminKeys()
	if keys[0].Name != "ops-laptop" || keys[0].Active {
		t.Errorf("after update = %+v", keys[0])
	}

	if err := d.DeleteAdminKey(k2.ID); err != nil {
		t.Fatal(err)
	}
	keys, _ = d.ListAdminKeys()
	if len(keys) != 1 {
		t.Fatalf("after delete = %d keys, want 1", len(keys))
	}
}