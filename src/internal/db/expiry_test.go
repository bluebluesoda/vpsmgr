package db

import "testing"

func TestUserExpiryRoundTrip(t *testing.T) {
	d := openTestDB(t)
	const exp = "2030-01-02T03:04:05Z"
	u, err := d.CreateUserFull("alice", "h", "10.115.0.2", 1, 30001, 10000, 1, 1024, 10, 0, StatusReady, "", exp)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.GetUserByName("alice")
	if err != nil {
		t.Fatal(err)
	}
	if got.ExpiresAt != exp {
		t.Errorf("GetUserByName ExpiresAt = %q, want %q", got.ExpiresAt, exp)
	}

	// A permanent account (empty expiry) reads back empty.
	if _, err := d.CreateUser("bob", "h", "10.115.0.3", 2, 30002, 10100, 1, 1024, 10); err != nil {
		t.Fatal(err)
	}
	bob, err := d.GetUserByName("bob")
	if err != nil {
		t.Fatal(err)
	}
	if bob.ExpiresAt != "" {
		t.Errorf("bob ExpiresAt = %q, want empty", bob.ExpiresAt)
	}

	// Clearing the deadline.
	if err := d.UpdateUserExpiry(u.ID, ""); err != nil {
		t.Fatal(err)
	}
	got, _ = d.GetUserByName("alice")
	if got.ExpiresAt != "" {
		t.Errorf("after clear ExpiresAt = %q, want empty", got.ExpiresAt)
	}

	// ListUsers carries the column too.
	if err := d.UpdateUserExpiry(u.ID, exp); err != nil {
		t.Fatal(err)
	}
	users, err := d.ListUsers()
	if err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, x := range users {
		if x.Name == "alice" && x.ExpiresAt == exp {
			seen = true
		}
	}
	if !seen {
		t.Errorf("ListUsers did not return alice's expiry %q: %+v", exp, users)
	}
}
