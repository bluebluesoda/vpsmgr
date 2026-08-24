package db

import (
	"path/filepath"
	"testing"
)

func TestImpersonatedSession(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	u, err := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}

	// A normal login session is not impersonated.
	ns, err := d.CreateSession(u.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	gu, imp, err := d.SessionWithFlag(ns.Token)
	if err != nil {
		t.Fatal(err)
	}
	if gu.ID != u.ID || imp {
		t.Errorf("normal session: user=%d imp=%v, want user=%d imp=false", gu.ID, imp, u.ID)
	}

	// An operator "log in as user" session carries the impersonated flag.
	is, err := d.CreateImpersonatedSession(u.ID, 3)
	if err != nil {
		t.Fatal(err)
	}
	gu, imp, err = d.SessionWithFlag(is.Token)
	if err != nil {
		t.Fatal(err)
	}
	if gu.ID != u.ID || !imp {
		t.Errorf("impersonated session: user=%d imp=%v, want imp=true", gu.ID, imp)
	}

	// The compat accessor still resolves the user either way.
	if _, err := d.SessionUser(ns.Token); err != nil {
		t.Errorf("SessionUser(normal): %v", err)
	}
	if _, err := d.SessionUser(is.Token); err != nil {
		t.Errorf("SessionUser(impersonated): %v", err)
	}
}
