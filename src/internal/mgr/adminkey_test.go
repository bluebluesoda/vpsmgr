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

	// Full-set resubmit: rename + "deactivate" the first, drop the second.
	// The admin panel no longer has an active toggle, so every key stays
	// active regardless of the submitted flag.
	keys, err = m.SaveAdminKeys([]SSHKeyInput{
		{ID: keys[0].ID, Name: "renamed", Key: ed25519Key, Active: false},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 1 {
		t.Fatalf("after reconcile = %d keys, want 1", len(keys))
	}
	if keys[0].Name != "renamed" || !keys[0].Active {
		t.Errorf("key not updated (should stay active): %+v", keys[0])
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

func TestSaveAdminKeyGrants(t *testing.T) {
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
	keys, err := m.SaveAdminKeys([]SSHKeyInput{
		{Name: "ops", Key: ed25519Key, Active: true},
		{Name: "ci", Key: ed25519Key2, Active: false},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Grant only the first admin key.
	granted, err := m.SaveAdminKeyGrants("alice", []int64{keys[0].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(granted) != 1 || granted[0].ID != keys[0].ID {
		t.Fatalf("granted = %+v, want only %d", granted, keys[0].ID)
	}

	// Stale/unknown ids are dropped; duplicates collapse to one grant.
	granted, err = m.SaveAdminKeyGrants("alice", []int64{999999, keys[1].ID, keys[1].ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(granted) != 1 || granted[0].ID != keys[1].ID {
		t.Fatalf("after stale/dedup = %+v, want only %d", granted, keys[1].ID)
	}

	// Empty selection revokes everything.
	granted, err = m.SaveAdminKeyGrants("alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(granted) != 0 {
		t.Fatalf("after clear = %d grants, want 0", len(granted))
	}
}

func TestApplySSHKeysScriptSync(t *testing.T) {
	user := []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBODYuserkeybogusbogusbogusX"}
	admin := []db.AdminKey{{Key: ed25519Key}}
	script := applySSHKeysScript(user, admin)

	// Every admin-managed line (carrying the marker) is dropped first, so a
	// key the user unchecks or the operator deletes no longer lingers.
	if !strings.Contains(script, "sed -i '/ "+AdminKeyMarker+"$/d' \"$AUTH\"") {
		t.Errorf("script missing stale-admin-line removal:\n%s", script)
	}
	// The activated admin key is re-appended with the marker.
	wantAdmin := ed25519Key + " " + AdminKeyMarker
	if !strings.Contains(script, "printf '%s\\n' '"+wantAdmin+"'") {
		t.Errorf("script missing activated admin key:\n%s", script)
	}
	// The user's own key is appended WITHOUT the marker.
	if !strings.Contains(script, "printf '%s\\n' '"+user[0]+"'") {
		t.Errorf("script missing user key:\n%s", script)
	}
	if strings.Contains(script, "printf '%s\\n' '"+user[0]+" "+AdminKeyMarker) {
		t.Error("user's own key must not carry the admin marker")
	}

	// A non-activated admin key (absent from adminKeys) must NOT be appended —
	// only the sed removal targets it. Nothing references it in the script.
	scriptEmpty := applySSHKeysScript(user, nil)
	if strings.Contains(scriptEmpty, ed25519Key) {
		t.Errorf("script should not append a non-activated admin key:\n%s", scriptEmpty)
	}
	// But the sed removal line is still there, so a previously injected copy
	// of that key gets purged on save.
	if !strings.Contains(scriptEmpty, "sed -i '/ "+AdminKeyMarker+"$/d'") {
		t.Errorf("script must still purge stale admin lines:\n%s", scriptEmpty)
	}
}

// TestAdminKeyMarkerSafe guards the injection-safety invariant: the marker is
// embedded inside single quotes in a shell script (see applySSHKeysScript) and
// inside a sed address, so it must contain no single quote, backslash, newline
// or slash that could break out.
func TestAdminKeyMarkerSafe(t *testing.T) {
	if AdminKeyMarker == "" {
		t.Fatal("AdminKeyMarker must not be empty")
	}
	if strings.ContainsAny(AdminKeyMarker, "'\\\n/") {
		t.Errorf("AdminKeyMarker %q contains shell-breaking characters", AdminKeyMarker)
	}
}