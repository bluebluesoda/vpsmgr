package mgr

import (
	"path/filepath"
	"strings"
	"testing"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
)

const ed25519Key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBODYbogusbogusbogusbogusbogusX"

const ed25519Key2 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHdifferenthashbogusbogusbogusQ"

func TestParsePublicKey(t *testing.T) {
	cases := []struct {
		in      string
		wantKey string
		wantCmt string
		ok      bool
	}{
		{ed25519Key, ed25519Key, "", true},
		{ed25519Key + " generated-by-termius", ed25519Key, "generated-by-termius", true},
		{ed25519Key + " user@host extra", ed25519Key, "user@host extra", true},
		{"ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQYlongerbodycKmGA64EMmZ1 test", "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQYlongerbodycKmGA64EMmZ1", "test", true},
		{"ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBFEZ", "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBFEZ", "", true},
		// invalid
		{"", "", "", false},
		{"ssh-ed25519 AAAA", "", "", false},                   // body too short
		{"ssh-ed25519 AAAA!!notbase64!!", "", "", false},      // bad body chars
		{"ssh-ed25519 AAAA bogus\n; rm -rf /", "", "", false}, // shell chars are not base64 but the regex body is greedy over base64... see below
		{"random text", "", "", false},
		{"ssh-rsa", "", "", false},
	}
	for _, c := range cases {
		got, cmt, ok := ParsePublicKey(c.in)
		if ok != c.ok {
			t.Errorf("%q: ok = %v, want %v", c.in, ok, c.ok)
			continue
		}
		if ok {
			if got != c.wantKey {
				t.Errorf("%q: key = %q, want %q", c.in, got, c.wantKey)
			}
			if cmt != c.wantCmt {
				t.Errorf("%q: comment = %q, want %q", c.in, cmt, c.wantCmt)
			}
		}
	}
}

func TestSaveSSHKeys(t *testing.T) {
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

	// Add two keys: one named, one unnamed (name falls back to the comment).
	keys, err := m.SaveSSHKeys("alice", []SSHKeyInput{
		{Name: "laptop", Key: ed25519Key, Active: true},
		{Name: "", Key: ed25519Key2 + " office-machine", Active: true},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("after add = %d keys, want 2", len(keys))
	}
	if keys[1].Name != "office-machine" {
		t.Errorf("comment fallback name = %q, want office-machine", keys[1].Name)
	}
	for _, k := range keys {
		if strings.Contains(k.Key, "office-machine") {
			t.Errorf("stored key still carries the comment: %q", k.Key)
		}
	}

	// A raw pasted key with a comment is stored clean. The panel always
	// resubmits the whole set, so carry the two existing keys by ID. The
	// comment "test@machine" contains a non-base64 char, so it is stripped.
	keys, err = m.SaveSSHKeys("alice", []SSHKeyInput{
		{ID: keys[0].ID, Name: keys[0].Name, Key: keys[0].Key, Active: keys[0].Active},
		{ID: keys[1].ID, Name: keys[1].Name, Key: keys[1].Key, Active: keys[1].Active},
		{Name: "ci", Key: "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQYlongerbodycKmGA64EMmZ1 test@machine", Active: true},
	})
	if err != nil {
		t.Fatalf("add raw: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("after raw add = %d keys, want 3", len(keys))
	}
	if keys[2].Key != "ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQYlongerbodycKmGA64EMmZ1" {
		t.Errorf("raw key not cleaned: %q", keys[2].Key)
	}

	// Duplicate clean key in the submitted set is rejected.
	_, err = m.SaveSSHKeys("alice", []SSHKeyInput{
		{ID: keys[0].ID, Name: keys[0].Name, Key: keys[0].Key, Active: keys[0].Active},
		{ID: keys[1].ID, Name: keys[1].Name, Key: keys[1].Key, Active: keys[1].Active},
		{ID: keys[2].ID, Name: keys[2].Name, Key: keys[2].Key, Active: keys[2].Active},
		{Name: "dup", Key: ed25519Key, Active: true},
	})
	if err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("duplicate should be rejected, got %v", err)
	}

	// Invalid key is rejected.
	_, err = m.SaveSSHKeys("alice", []SSHKeyInput{
		{ID: keys[0].ID, Name: keys[0].Name, Key: keys[0].Key, Active: keys[0].Active},
		{ID: keys[1].ID, Name: keys[1].Name, Key: keys[1].Key, Active: keys[1].Active},
		{ID: keys[2].ID, Name: keys[2].Name, Key: keys[2].Key, Active: keys[2].Active},
		{Name: "bad", Key: "not-a-key", Active: true},
	})
	if err == nil {
		t.Error("invalid key should be rejected")
	}

	// Empty submission deletes every key.
	keys, err = m.SaveSSHKeys("alice", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 0 {
		t.Fatalf("after clear = %d keys, want 0", len(keys))
	}
}

func TestSaveSSHKeysUpdatesAndDeletes(t *testing.T) {
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

	k1, err := m.SaveSSHKeys("alice", []SSHKeyInput{
		{Name: "a", Key: ed25519Key, Active: true},
		{Name: "b", Key: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIBODYotherboguskeyboguskeybogusX", Active: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Resubmit only k1, renamed and deactivated: k2 must be deleted, k1 updated.
	keys, err := m.SaveSSHKeys("alice", []SSHKeyInput{
		{ID: k1[0].ID, Name: "renamed", Key: ed25519Key, Active: false},
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
	// k2 is gone.
	active, err := m.ActiveKeys("alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(active) != 0 {
		t.Errorf("deactivated key still active: %v", active)
	}
}
