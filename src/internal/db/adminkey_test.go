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

func TestAdminKeyGrants(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	u, err := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	a1, err := d.AddAdminKey("ops", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBODYadminkeybogusX", true)
	if err != nil {
		t.Fatal(err)
	}
	a2, err := d.AddAdminKey("ci", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBODYadminkeybogusY", false)
	if err != nil {
		t.Fatal(err)
	}
	a3, err := d.AddAdminKey("temp", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBODYadminkeybogusZ", true)
	if err != nil {
		t.Fatal(err)
	}

	if err := d.SetAdminKeyGrants(u.ID, []int64{a1.ID, a2.ID}); err != nil {
		t.Fatal(err)
	}
	granted, err := d.GrantedAdminKeys(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(granted) != 2 || granted[0].ID != a1.ID || granted[1].ID != a2.ID {
		t.Fatalf("granted = %+v, want [%d %d]", granted, a1.ID, a2.ID)
	}
	// The joined row carries the admin key's live content and name.
	if granted[0].Name != "ops" || granted[0].Key == "" {
		t.Errorf("granted row = %+v", granted[0])
	}

	// Replacing the set wholesale drops the first grant.
	if err := d.SetAdminKeyGrants(u.ID, []int64{a2.ID}); err != nil {
		t.Fatal(err)
	}
	granted, _ = d.GrantedAdminKeys(u.ID)
	if len(granted) != 1 || granted[0].ID != a2.ID {
		t.Fatalf("after replace = %+v, want only %d", granted, a2.ID)
	}

	// Deleting an admin key cascades away its grants.
	if err := d.SetAdminKeyGrants(u.ID, []int64{a2.ID, a3.ID}); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteAdminKey(a3.ID); err != nil {
		t.Fatal(err)
	}
	granted, _ = d.GrantedAdminKeys(u.ID)
	if len(granted) != 1 || granted[0].ID != a2.ID {
		t.Fatalf("after admin-key delete = %+v, want only %d", granted, a2.ID)
	}

	// Empty set clears everything.
	if err := d.SetAdminKeyGrants(u.ID, nil); err != nil {
		t.Fatal(err)
	}
	granted, _ = d.GrantedAdminKeys(u.ID)
	if len(granted) != 0 {
		t.Fatalf("after clear = %d grants, want 0", len(granted))
	}
}