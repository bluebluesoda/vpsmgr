package mgr

import (
	"path/filepath"
	"strings"
	"testing"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
)

func TestSaveAdminKeys(t *testing.T) {
	c := cfg.Default()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	m := New(c, d)

	keys, err := m.SaveAdminKeys([]SSHKeyInput{
		{Name: "ops", Key: ed25519Key, Active: true},
		{Name: "", Key: ed25519Key2 + " ci-host", Active: true},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("after add = %d keys, want 2", len(keys))
	}
	if keys[1].Name != "ci-host" {
		t.Errorf("comment fallback name = %q, want ci-host", keys[1].Name)
	}
	for _, k := range keys {
		if strings.Contains(k.Key, "ci-host") {
			t.Errorf("stored key still carries the comment: %q", k.Key)
		}
	}

	// Full-set resubmit: rename + deactivate the first, drop the second.
	keys, err = m.SaveAdminKeys([]SSHKeyInput{
		{ID: keys[0].ID, Name: "renamed", Key: ed25519Key, Active: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("after reconcile = %d keys, want 1", len(keys))
	}
	if keys[0].Name != "renamed" || keys[0].Active {
		t.Errorf("key not updated: %+v", keys[0])
	}

	// Duplicate in the submitted set is rejected.
	_, err = m.SaveAdminKeys([]SSHKeyInput{
		{Name: "a", Key: ed25519Key, Active: true},
		{Name: "b", Key: ed25519Key, Active: true},
	})
	if err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("duplicate should be rejected, got %v", err)
	}

	// Invalid key is rejected.
	if _, err := m.SaveAdminKeys([]SSHKeyInput{
		{Name: "bad", Key: "not-a-key", Active: true},
	}); err == nil {
		t.Error("invalid key should be rejected")
	}

	// Empty submission clears everything.
	keys, err = m.SaveAdminKeys(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("after clear = %d keys, want 0", len(keys))
	}
}