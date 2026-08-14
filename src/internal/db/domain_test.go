package db

import "testing"

func TestDomainProtocolAndTimestamps(t *testing.T) {
	d, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	u, err := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	dmn, err := d.AddDomain(u.ID, "example.com", true)
	if err != nil {
		t.Fatal(err)
	}
	if !dmn.ProxyProtocol {
		t.Fatal("proxy_protocol not stored")
	}
	if dmn.UpdatedAt == "" || dmn.UpdatedAt != dmn.CreatedAt {
		t.Errorf("updated_at=%q created_at=%q", dmn.UpdatedAt, dmn.CreatedAt)
	}
	// SetDomainProtocol flips the flag and (re)bumps updated_at (second
	// resolution UTC — a same-second change may keep the same value).
	if err := d.SetDomainProtocol(dmn.ID, false); err != nil {
		t.Fatal(err)
	}
	got, err := d.GetDomain(u.ID, "example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProxyProtocol {
		t.Error("proxy_protocol should be false after SetDomainProtocol")
	}
	if got.UpdatedAt == "" {
		t.Error("updated_at empty after SetDomainProtocol")
	}
	// GetDomainByDomain finds it globally.
	byD, err := d.GetDomainByDomain("example.com")
	if err != nil {
		t.Fatal(err)
	}
	if byD.UserID != u.ID || byD.ProxyProtocol {
		t.Errorf("GetDomainByDomain = %+v", byD)
	}
}

func TestListAllDomainsJoinsUser(t *testing.T) {
	d, err := Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	alice, _ := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10)
	bob, _ := d.CreateUser("bob", "h", "10.115.0.3", 2, 30002, 10100, 1, 1024, 10)
	if _, err := d.AddDomain(alice.ID, "zebra.com", false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.AddDomain(bob.ID, "alpha.com", true); err != nil {
		t.Fatal(err)
	}
	// alpha.com is modified later, so it sorts first (updated_at DESC, id DESC).
	alpha, _ := d.GetDomainByDomain("alpha.com")
	if err := d.SetDomainProtocol(alpha.ID, true); err != nil {
		t.Fatal(err)
	}
	all, err := d.ListAllDomains()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d domains, want 2", len(all))
	}
	if all[0].Domain != "alpha.com" || all[0].Username != "bob" || all[0].IP != "10.115.0.3" || !all[0].ProxyProtocol {
		t.Errorf("first row = %+v", all[0])
	}
	if all[1].Domain != "zebra.com" || all[1].Username != "alice" {
		t.Errorf("second row = %+v", all[1])
	}
}
