package mgr

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
)

func batchTestManager(t *testing.T) (*Manager, *db.DB) {
	t.Helper()
	c := cfg.Default()
	c.Incus.Socket = "/nonexistent/vpsmgr-test.sock"
	d, err := db.Open(filepath.Join(t.TempDir(), "batch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return New(c, d), d
}

func TestValidateBatchNames(t *testing.T) {
	m, d := batchTestManager(t)
	if _, err := d.CreateUser("taken", "h", "10.115.0.9", 9, 30009, 10900, 10, 1024, 10); err != nil {
		t.Fatal(err)
	}

	// Cleaning: blanks are padding, names fold to lowercase.
	got, err := m.ValidateBatchNames([]string{"  Alice  ", "", "bob", "   "})
	if err != nil {
		t.Fatalf("valid list rejected: %v", err)
	}
	if len(got) != 2 || got[0] != "alice" || got[1] != "bob" {
		t.Fatalf("cleaned names = %v, want [alice bob]", got)
	}

	bad := []struct {
		name  string
		names []string
		want  string
	}{
		{"empty", []string{"", "  "}, "no usernames given"},
		{"invalid syntax", []string{"9lead"}, "line 1"},
		{"trailing hyphen", []string{"nope-"}, "line 1"},
		{"duplicate", []string{"dup", "DUP"}, "listed twice"},
		{"already exists", []string{"taken"}, "already exists"},
	}
	for _, tc := range bad {
		if _, err := m.ValidateBatchNames(tc.names); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}

	// An all-or-nothing check: one bad line rejects the whole list, so nothing
	// of the good part is created.
	if _, err := m.ValidateBatchNames([]string{"goodone", "9bad"}); err == nil {
		t.Error("a bad line must reject the whole batch")
	}

	// The size cap keeps a pasted wall of text from churning the host.
	var many []string
	for i := 0; i <= MaxBatchUsers; i++ {
		many = append(many, fmt.Sprintf("user%03da", i))
	}
	if _, err := m.ValidateBatchNames(many); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("oversized batch err = %v, want a cap error", err)
	}
}

// A batch reports every user exactly twice (running, then failed) and keeps
// going: one broken user must not abandon the rest of the list.
func TestAddBatchReportsEveryUser(t *testing.T) {
	m, _ := batchTestManager(t)

	var seen []string
	m.AddBatch([]string{"alpha", "beta"},
		AddOptions{CPU: 10, MemMB: 1024, DiskGB: 10, AllowChild: true, FromShare: "no-such-code"},
		nil,
		func(r BatchResult) { seen = append(seen, r.Name+":"+r.State) })

	want := []string{"alpha:" + BatchRunning, "alpha:" + BatchFailed, "beta:" + BatchRunning, "beta:" + BatchFailed}
	if len(seen) != len(want) {
		t.Fatalf("reports = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("reports = %v, want %v", seen, want)
		}
	}
}

func TestAddFromShareRequiresSharingEnabled(t *testing.T) {
	m, _ := batchTestManager(t)
	if m.SnapshotShareEnabled() {
		t.Fatal("sharing must default to disabled")
	}
	_, err := m.Add("cloned", AddOptions{CPU: 10, MemMB: 1024, DiskGB: 10, FromShare: "some-code"})
	if err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("Add from share while disabled = %v, want a disabled error", err)
	}
}

// With sharing on, a bogus code is rejected before any allocation happens.
func TestAddFromShareRejectsUnknownCode(t *testing.T) {
	c := cfg.Default()
	c.Incus.Socket = "/nonexistent/vpsmgr-test.sock"
	c.Snapshots.Share = true
	d, err := db.Open(filepath.Join(t.TempDir(), "batch2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	m := New(c, d)

	_, err = m.Add("cloned", AddOptions{CPU: 10, MemMB: 1024, DiskGB: 10, FromShare: "not-a-real-code"})
	if err == nil || !strings.Contains(err.Error(), "invalid or expired") {
		t.Fatalf("unknown share code = %v, want invalid/expired", err)
	}
}
