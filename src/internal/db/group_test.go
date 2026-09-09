package db

import (
	"testing"

	"vpsmgr/internal/pw"
)

func TestUpdateUsersColorAtomically(t *testing.T) {
	d, err := Open(t.TempDir() + "/group.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	first, err := d.CreateUser("alice", "old-a", "10.42.0.2", 1, 30001, 10000, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.CreateUser("alice-1", "old-b", "10.42.0.3", 2, 30002, 10100, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateUsersColor([]int64{first.ID, second.ID}, "#16a34a"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{first.ID, second.ID} {
		u, err := d.GetUserByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if u.Color != "#16a34a" {
			t.Errorf("user %d color = %q, want #16a34a", id, u.Color)
		}
	}
}

func TestUpdateUsersPasswordAndDeleteSessions(t *testing.T) {
	d, err := Open(t.TempDir() + "/group.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	first, err := d.CreateUser("alice", "old-a", "10.42.0.2", 1, 30001, 10000, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.CreateUser("alice-1", "old-b", "10.42.0.3", 2, 30002, 10100, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	keep, err := d.CreateSession(first.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	old, err := d.CreateSession(second.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := pw.Hash("new-password")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateUsersPasswordAndDeleteSessions([]int64{first.ID, second.ID}, hash, keep.Token); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{first.ID, second.ID} {
		u, err := d.GetUserByID(id)
		if err != nil {
			t.Fatal(err)
		}
		if !pw.Verify(u.PassHash, "new-password") {
			t.Errorf("user %d password was not updated", id)
		}
	}
	if _, _, err := d.SessionWithFlag(keep.Token); err != nil {
		t.Fatalf("kept session rejected: %v", err)
	}
	if _, _, err := d.SessionWithFlag(old.Token); err == nil {
		t.Fatal("old sibling session remained valid")
	}
}
