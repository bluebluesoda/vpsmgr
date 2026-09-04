package db

import (
	"path/filepath"
	"testing"
)

func TestSSHKeyCRUD(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	u, err := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	k1, err := d.AddSSHKey(u.ID, "laptop", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBODY", true)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := d.AddSSHKey(u.ID, "ci", "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQY", false)
	if err != nil {
		t.Fatal(err)
	}
	if k1.ID == 0 || k2.ID == 0 {
		t.Fatal("expected autoincrement ids")
	}

	keys, err := d.ListSSHKeys(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("ListSSHKeys = %d keys, want 2", len(keys))
	}
	if !keys[0].Active || keys[1].Active {
		t.Errorf("active flags wrong: %+v %+v", keys[0].Active, keys[1].Active)
	}

	// Active filter returns only the enabled key.
	active, err := d.ActiveSSHKeys(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 1 || active[0].ID != k1.ID {
		t.Fatalf("ActiveSSHKeys = %+v, want only k1", active)
	}

	// Update toggles active and renames.
	if err := d.UpdateSSHKey(k1.ID, "work-laptop", k1.Key, false); err != nil {
		t.Fatal(err)
	}
	keys, _ = d.ListSSHKeys(u.ID)
	if keys[0].Name != "work-laptop" || keys[0].Active {
		t.Errorf("after update = %+v", keys[0])
	}

	// Delete removes one row.
	if err := d.DeleteSSHKey(k2.ID); err != nil {
		t.Fatal(err)
	}
	keys, _ = d.ListSSHKeys(u.ID)
	if len(keys) != 1 {
		t.Fatalf("after delete = %d keys, want 1", len(keys))
	}
}

func TestSSHKeysCascadeOnUserDelete(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	u, err := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddSSHKey(u.ID, "laptop", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBODY", true); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteUser(u.ID); err != nil {
		t.Fatal(err)
	}
	keys, err := d.ListSSHKeys(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("keys survived user delete: %+v", keys)
	}
}
