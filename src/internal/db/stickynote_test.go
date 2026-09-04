package db

import (
	"path/filepath"
	"testing"
)

func TestStickyNotesRoundTrip(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// A user with no notes: GetStickyNotes returns "" (not an error).
	hash := "h"
	u, err := d.CreateUser("alice", hash, "10.42.0.2", 1, 30001, 10000, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.GetStickyNotes(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("fresh user notes = %q, want empty", got)
	}

	blob := `{"v":1,"salt":"c2FsdA==","iv":"aXZlYg==","ct":"Y3Q="}`
	if err := d.SetStickyNotes(u.ID, blob); err != nil {
		t.Fatal(err)
	}
	got, err = d.GetStickyNotes(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got != blob {
		t.Errorf("notes = %q, want %q", got, blob)
	}

	// Overwrite in place.
	blob2 := `{"v":1,"salt":"Yg==","iv":"Yw==","ct":"ZA=="}`
	if err := d.SetStickyNotes(u.ID, blob2); err != nil {
		t.Fatal(err)
	}
	got, _ = d.GetStickyNotes(u.ID)
	if got != blob2 {
		t.Errorf("overwritten notes = %q, want %q", got, blob2)
	}

	// Clearing returns to the un-enabled state.
	if err := d.SetStickyNotes(u.ID, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = d.GetStickyNotes(u.ID)
	if got != "" {
		t.Errorf("cleared notes = %q, want empty", got)
	}

	// Users are independent.
	if _, err := d.CreateUser("bob", hash, "10.42.0.3", 2, 30002, 10100, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	if err := d.SetStickyNotes(u.ID, blob); err != nil {
		t.Fatal(err)
	}
	bob, err := d.GetUserByName("bob")
	if err != nil {
		t.Fatal(err)
	}
	if bobNotes, _ := d.GetStickyNotes(bob.ID); bobNotes != "" {
		t.Errorf("bob inherited notes: %q", bobNotes)
	}
}
