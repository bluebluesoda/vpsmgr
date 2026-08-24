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

	// Every panel-managed line is dropped first (both markers), so a key the
	// user unchecks/deletes no longer lingers.
	if !strings.Contains(script, "sed -i '/ "+UserKeyMarker+"$/d' \"$AUTH\"") {
		t.Errorf("script missing user-key removal:\n%s", script)
	}
	if !strings.Contains(script, "sed -i '/ "+AdminKeyMarker+"$/d' \"$AUTH\"") {
		t.Errorf("script missing admin-key removal:\n%s", script)
	}
	// The activated admin key is re-appended with its marker.
	wantAdmin := ed25519Key + " " + AdminKeyMarker
	if !strings.Contains(script, "printf '%s\\n' '"+wantAdmin+"'") {
		t.Errorf("script missing activated admin key:\n%s", script)
	}
	// The user's own active key is re-appended with ITS marker.
	wantUser := user[0] + " " + UserKeyMarker
	if !strings.Contains(script, "printf '%s\\n' '"+wantUser+"'") {
		t.Errorf("script missing user key (marked):\n%s", script)
	}
	if strings.Contains(script, "printf '%s\\n' '"+user[0]+"'") {
		t.Error("user's own key must carry the user marker (not be unmarked)")
	}

	// A non-activated admin key (absent from adminKeys) must NOT be appended —
	// only the sed removal targets it. Nothing references it in the script.
	scriptEmpty := applySSHKeysScript(user, nil)
	if strings.Contains(scriptEmpty, ed25519Key) {
		t.Errorf("script should not append a non-activated admin key:\n%s", scriptEmpty)
	}
	// But the sed removal lines are still there, so a previously injected copy
	// of that key gets purged on save.
	if !strings.Contains(scriptEmpty, "sed -i '/ "+AdminKeyMarker+"$/d'") {
		t.Errorf("script must still purge stale admin lines:\n%s", scriptEmpty)
	}
}

// TestApplySSHKeysScriptSameKeyBothRoles covers the tricky case where the SAME
// public key is simultaneously a user's own active key AND a granted admin key.
// They are written as two independent marked lines (user marker vs admin
// marker), so the key is effective regardless of which role activated it —
// there is no ordering bug that could make it ineffective.
func TestApplySSHKeysScriptSameKeyBothRoles(t *testing.T) {
	// Case A: same key is the user's own active key AND a granted admin key.
	both := applySSHKeysScript([]string{ed25519Key}, []db.AdminKey{{Key: ed25519Key}})
	if !strings.Contains(both, "printf '%s\\n' '"+ed25519Key+" "+UserKeyMarker+"'") {
		t.Errorf("user's own copy must carry the user marker:\n%s", both)
	}
	if !strings.Contains(both, "printf '%s\\n' '"+ed25519Key+" "+AdminKeyMarker+"'") {
		t.Errorf("granted admin copy must carry the admin marker:\n%s", both)
	}

	// Case B: same key only active as the user's OWN key (admin copy not
	// granted) -> only the user-marked line is appended.
	ownOnly := applySSHKeysScript([]string{ed25519Key}, nil)
	if !strings.Contains(ownOnly, "printf '%s\\n' '"+ed25519Key+" "+UserKeyMarker+"'") {
		t.Errorf("user-active-only key must be appended:\n%s", ownOnly)
	}
	if strings.Contains(ownOnly, ed25519Key+" "+AdminKeyMarker) {
		t.Errorf("user-active-only must not append an admin-marked line:\n%s", ownOnly)
	}

	// Case C: same key only active as a GRANTED admin key (not in the user's
	// own list) -> only the admin-marked line is appended; SSH ignores the
	// comment so it still authorizes the key.
	grantOnly := applySSHKeysScript(nil, []db.AdminKey{{Key: ed25519Key}})
	if !strings.Contains(grantOnly, "printf '%s\\n' '"+ed25519Key+" "+AdminKeyMarker+"'") {
		t.Errorf("grant-only key must be appended marked:\n%s", grantOnly)
	}
	if strings.Contains(grantOnly, "printf '%s\\n' '"+ed25519Key+"'") {
		t.Errorf("grant-only must not append an unmarked user line:\n%s", grantOnly)
	}
}

// TestKeyMarkersSafe guards the injection-safety invariant: both markers are
// embedded inside single quotes in a shell script (see applySSHKeysScript) and
// inside sed addresses, so each must contain no single quote, backslash,
// newline or slash that could break out. They must also be distinct (and not
// end-of-line equivalent) so purging one never removes the other.
func TestKeyMarkersSafe(t *testing.T) {
	for _, m := range []string{UserKeyMarker, AdminKeyMarker} {
		if m == "" {
			t.Fatal("markers must not be empty")
		}
		if strings.ContainsAny(m, "'\\\n/") {
			t.Errorf("marker %q contains shell-breaking characters", m)
		}
	}
	if UserKeyMarker == AdminKeyMarker {
		t.Fatal("user and admin markers must be distinct")
	}
}