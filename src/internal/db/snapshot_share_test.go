package db

import "testing"

func TestSnapshotShareStore(t *testing.T) {
	d, err := Open(t.TempDir() + "/share.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	alice, err := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 10, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}

	// No share yet.
	if s, err := d.GetSnapshotShare(alice.ID); err != nil || s != nil {
		t.Fatalf("unset share = %+v, err=%v, want nil", s, err)
	}

	if err := d.SetSnapshotShare(alice.ID, "code-1", "snap-a"); err != nil {
		t.Fatal(err)
	}
	s, err := d.GetSnapshotShare(alice.ID)
	if err != nil || s == nil || s.Code != "code-1" || s.Snapshot != "snap-a" {
		t.Fatalf("share = %+v, err=%v", s, err)
	}
	if byCode, err := d.GetSnapshotShareByCode("code-1"); err != nil || byCode == nil || byCode.UserID != alice.ID {
		t.Fatalf("by code = %+v, err=%v", byCode, err)
	}
	if byCode, err := d.GetSnapshotShareByCode("nope"); err != nil || byCode != nil {
		t.Fatalf("unknown code = %+v, err=%v, want nil", byCode, err)
	}

	// Publishing again replaces the previous share (one per user).
	if err := d.SetSnapshotShare(alice.ID, "code-2", "snap-b"); err != nil {
		t.Fatal(err)
	}
	if byCode, _ := d.GetSnapshotShareByCode("code-1"); byCode != nil {
		t.Error("old code still resolves after republish")
	}
	if s, _ := d.GetSnapshotShare(alice.ID); s == nil || s.Code != "code-2" {
		t.Fatalf("share not replaced: %+v", s)
	}

	// Deleting a different snapshot leaves the share alone; the matching one
	// clears it.
	if err := d.DeleteSnapshotShareBySnapshot(alice.ID, "snap-x"); err != nil {
		t.Fatal(err)
	}
	if s, _ := d.GetSnapshotShare(alice.ID); s == nil {
		t.Fatal("share cleared by an unrelated snapshot")
	}
	if err := d.DeleteSnapshotShareBySnapshot(alice.ID, "snap-b"); err != nil {
		t.Fatal(err)
	}
	if s, _ := d.GetSnapshotShare(alice.ID); s != nil {
		t.Fatal("share not cleared for its own snapshot")
	}
}

func TestSnapshotShareMirror(t *testing.T) {
	d, err := Open(t.TempDir() + "/share-mirror.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// Unset: ok=false so the caller falls back to the config file.
	if _, ok, err := d.SnapshotShareEnabled(); err != nil || ok {
		t.Fatalf("unset mirror: ok=%v err=%v", ok, err)
	}
	if err := d.SetSnapshotShareEnabled(false); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := d.SnapshotShareEnabled(); !ok || v != "0" {
		t.Fatalf("mirror after disable = %q, ok=%v", v, ok)
	}
	if err := d.SetSnapshotShareEnabled(true); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := d.SnapshotShareEnabled(); !ok || v != "1" {
		t.Fatalf("mirror after enable = %q, ok=%v", v, ok)
	}
}

func TestSnapshotShareCascadeOnUserDelete(t *testing.T) {
	d, err := Open(t.TempDir() + "/share-cascade.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	u, err := d.CreateUser("bob", "h", "10.115.0.3", 2, 30002, 10100, 10, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetSnapshotShare(u.ID, "code-3", "snap-c"); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteUser(u.ID); err != nil {
		t.Fatal(err)
	}
	if s, err := d.GetSnapshotShareByCode("code-3"); err != nil || s != nil {
		t.Fatalf("share survived user deletion: %+v, err=%v", s, err)
	}
}
