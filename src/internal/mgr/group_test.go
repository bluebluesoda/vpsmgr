package mgr

import (
	"testing"

	"vpsmgr/internal/db"
)

func TestParseUserGroup(t *testing.T) {
	tests := []struct {
		name   string
		parent string
		child  bool
	}{
		{name: "alice", parent: "", child: false},
		{name: "alice-1", parent: "alice", child: true},
		{name: "alice-12", parent: "alice", child: true},
		{name: "my-box-3", parent: "my-box", child: true},
		{name: "alice-1-2", parent: "", child: false},
		{name: "alice1", parent: "", child: false},
	}
	for _, tt := range tests {
		got := ParseUserGroup(tt.name)
		if got.Parent != tt.parent || got.Child != tt.child {
			t.Fatalf("ParseUserGroup(%q) = %+v, want parent=%q child=%v", tt.name, got, tt.parent, tt.child)
		}
	}
}

func TestUserGroupLabel(t *testing.T) {
	for _, test := range []struct {
		name, want string
	}{
		{"user", "A"},
		{"user-1", "1"},
		{"user-09", "09"},
		{"user-0928", "0928"},
	} {
		if got := UserGroupLabel(test.name); got != test.want {
			t.Errorf("UserGroupLabel(%q) = %q, want %q", test.name, got, test.want)
		}
	}
}

func TestMachineSpecs(t *testing.T) {
	for _, test := range []struct {
		cpu, mem, disk int
		want           string
	}{
		{10, 1024, 10, "1c 1g 10g"},
		{40, 8192, 40, "4c 8g 40g"},
		{5, 512, 10, "0.5c 0.5g 10g"},
		{20, 1536, 20, "2c 1.5g 20g"},
	} {
		if got := MachineSpecs(test.cpu, test.mem, test.disk); got != test.want {
			t.Errorf("MachineSpecs(%d, %d, %d) = %q, want %q", test.cpu, test.mem, test.disk, got, test.want)
		}
	}
}

func TestUserGroupName(t *testing.T) {
	if got := UserGroupName("alice-1"); got != "alice" {
		t.Fatalf("UserGroupName(alice-1) = %q, want alice", got)
	}
	if got := UserGroupName("alice"); got != "alice" {
		t.Fatalf("UserGroupName(alice) = %q, want alice", got)
	}
}

func TestValidateAddName(t *testing.T) {
	m := &Manager{}
	for _, name := range []string{"alice", "alice-1", "alice-99"} {
		if err := m.ValidateAddName(name, true); err != nil {
			t.Errorf("ValidateAddName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{"alice-1-2", "alice--1", "-1"} {
		if err := m.ValidateAddName(name, true); err == nil {
			t.Errorf("ValidateAddName(%q) = nil, want error", name)
		}
	}
	if err := m.ValidateAddName("alice-1", false); err == nil {
		t.Error("ValidateAddName child with allowChild=false = nil, want error")
	}
}

func TestUsersInGroup(t *testing.T) {
	d := testDB(t)
	for i, name := range []string{"alice", "alice-1", "alice-2", "bob"} {
		if _, err := d.CreateUser(name, "hash", "10.42.0."+string(rune('2'+i)), i+1, 30001+i, 10000+i*100, 1, 1024, 10); err != nil {
			t.Fatal(err)
		}
	}
	m := &Manager{db: d}
	users, err := m.UsersInGroup("alice-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(users) != 3 {
		t.Fatalf("UsersInGroup(alice-1) returned %d users, want 3", len(users))
	}
}

func testDB(t *testing.T) *db.DB {
	t.Helper()
	d, err := db.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}
