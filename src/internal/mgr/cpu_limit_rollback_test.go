package mgr

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
)

// fakeCPUApplier records every quota write so a test can assert both the cap and
// the rollback that follows a failed state save.
type fakeCPUApplier struct {
	mu    sync.Mutex
	calls []string

	// failRe applies to names matching this substring ("" = never fail).
	failRe string
}

func (f *fakeCPUApplier) SetCPU(name string, cpuTenths int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRe != "" && strings.Contains(name, f.failRe) {
		return fmt.Errorf("fake applier refuses %s", name)
	}
	f.calls = append(f.calls, fmt.Sprintf("%s:%d", name, cpuTenths))
	return nil
}

func (f *fakeCPUApplier) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// A cap whose state cannot be persisted must be lifted again: the DB row is what
// tells the enforcement loop a container is capped, so a cap without one would
// never expire and would survive switching the rule off.
func TestCPULimitRollsBackCapsThatCannotBeSaved(t *testing.T) {
	c := cfg.Default()
	c.Incus.Socket = "/nonexistent/vpsmgr-test.sock"
	path := filepath.Join(t.TempDir(), "cpu.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	u, err := d.CreateUser("dave", "h", "10.115.0.5", 4, 30004, 10300, 10, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	m := New(c, d)
	fake := &fakeCPUApplier{}
	m.cpuLimit = fake
	if err := m.SetCPULimitRule(CPULimitRule{
		Enabled: true, WindowMinutes: 2, Percent: 5, CoresX10: 2, DurationSeconds: 600,
	}); err != nil {
		t.Fatal(err)
	}
	// Two consecutive minutes at 90% of dave's own quota.
	now := time.Now().Unix()
	step := int64(ResourceSampleInterval / time.Second)
	sample := func(min int64) db.ResourceSample {
		return db.ResourceSample{UserID: u.ID, SampleMinute: min, State: 1, CPUPercentX10: 900}
	}
	if err := d.RecordResourceSamples(
		[]db.ResourceSample{sample(now - 2*step), sample(now - step), sample(now)}, nil, "", 0); err != nil {
		t.Fatal(err)
	}

	// Break the state write only: a trigger created over a second connection to
	// the same file rejects the active-limit row while reads keep working.
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	for _, ev := range []string{"INSERT", "UPDATE"} {
		if _, err := raw.Exec(fmt.Sprintf(
			`CREATE TRIGGER deny_%s BEFORE %s ON settings WHEN NEW.key='cpu_limit_active'
			 BEGIN SELECT RAISE(ABORT, 'simulated write failure'); END`, ev, ev)); err != nil {
			t.Fatal(err)
		}
	}

	err = m.EnforceCPULimits()
	if err == nil {
		t.Fatal("EnforceCPULimits succeeded although the state could not be saved")
	}
	if !strings.Contains(err.Error(), "save cpu limit state") {
		t.Errorf("error does not name the failed save: %v", err)
	}
	// The cap was applied and then lifted back to dave's own quota.
	if got, want := fake.seen(), []string{"dave:2", "dave:10"}; len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("quota writes = %v, want %v (cap, then rollback)", got, want)
	}
	// Nothing was recorded, so the next pass starts from the same place: the
	// rule still caps dave, and the write still fails.
	if active := m.CPULimits(); len(active) != 0 {
		t.Fatalf("active limits = %+v, want none after a failed save", active)
	}
	// Drop the trigger: the same pass now persists normally.
	for _, ev := range []string{"INSERT", "UPDATE"} {
		if _, err := raw.Exec(`DROP TRIGGER deny_` + ev); err != nil {
			t.Fatal(err)
		}
	}
	fake.calls = nil
	if err := m.EnforceCPULimits(); err != nil {
		t.Fatalf("enforcement after the write recovered: %v", err)
	}
	if got := fake.seen(); len(got) != 1 || got[0] != "dave:2" {
		t.Fatalf("quota writes = %v, want [dave:2]", got)
	}
	if active := m.CPULimits(); active["dave"].CoresX10 != 2 {
		t.Fatalf("active limits = %+v, want dave capped", active)
	}
}

// A rollback that itself fails is reported (the container keeps the cap, and the
// next pass re-evaluates it) instead of being silently dropped.
func TestCPULimitReportsFailedRollback(t *testing.T) {
	c := cfg.Default()
	c.Incus.Socket = "/nonexistent/vpsmgr-test.sock"
	d, err := db.Open(filepath.Join(t.TempDir(), "cpu2.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	u, err := d.CreateUser("dave", "h", "10.115.0.5", 4, 30004, 10300, 10, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	m := New(c, d)

	applied := []*db.User{u}
	fake := &fakeCPUApplier{failRe: "dave"}
	m.cpuLimit = fake
	err = m.rollbackCaps(applied, fmt.Errorf("save cpu limit state: boom"))
	if err == nil || !strings.Contains(err.Error(), "boom") ||
		!strings.Contains(err.Error(), "restore dave after an unsaved cpu limit") {
		t.Fatalf("rollback error = %v", err)
	}
}
