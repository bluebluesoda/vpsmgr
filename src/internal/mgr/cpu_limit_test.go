package mgr

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
)

func TestParseLimitCores(t *testing.T) {
	valid := map[string]int{"0.1": 1, "0.5": 5, "0.9": 9, "1": 10, "1.0": 10, ".5": 5}
	for in, want := range valid {
		got, err := ParseLimitCores(in)
		if err != nil {
			t.Errorf("ParseLimitCores(%q) error: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseLimitCores(%q) = %d, want %d", in, got, want)
		}
	}
	invalid := []string{"", "  ", "0", "0.0", "0.05", "1.1", "1.5", "2", "-0.5", "abc", "1e2"}
	for _, in := range invalid {
		if _, err := ParseLimitCores(in); err == nil {
			t.Errorf("ParseLimitCores(%q) accepted, want error", in)
		}
	}
}

func TestValidateCPULimitRule(t *testing.T) {
	valid := []CPULimitRule{
		{Enabled: true, WindowMinutes: 1, Percent: 100, CoresX10: 1, DurationSeconds: 60},
		{Enabled: true, WindowMinutes: 10, Percent: 60, CoresX10: 10, DurationSeconds: 9000},
		{Enabled: false, WindowMinutes: 10, Percent: 60, CoresX10: 5, DurationSeconds: 0},
	}
	for _, r := range valid {
		if err := ValidateCPULimitRule(r); err != nil {
			t.Errorf("ValidateCPULimitRule(%+v) error: %v", r, err)
		}
	}
	invalid := []CPULimitRule{
		{Enabled: true, WindowMinutes: 0, Percent: 60, CoresX10: 5, DurationSeconds: 60},
		{Enabled: true, WindowMinutes: 10, Percent: 0, CoresX10: 5, DurationSeconds: 60},
		{Enabled: true, WindowMinutes: 10, Percent: 101, CoresX10: 5, DurationSeconds: 60},
		{Enabled: true, WindowMinutes: 10, Percent: 60, CoresX10: 0, DurationSeconds: 60},
		{Enabled: true, WindowMinutes: 10, Percent: 60, CoresX10: 11, DurationSeconds: 60},
		{Enabled: true, WindowMinutes: 10, Percent: 60, CoresX10: 5, DurationSeconds: 0},
		{Enabled: false, WindowMinutes: 10, Percent: 60, CoresX10: 5, DurationSeconds: -1},
	}
	for _, r := range invalid {
		if err := ValidateCPULimitRule(r); err == nil {
			t.Errorf("ValidateCPULimitRule(%+v) accepted, want error", r)
		}
	}
}

func TestCPULimitRuleRoundTrip(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "rule.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	m := New(cfg.Default(), d)

	// Unset: the disabled prefilled default is returned.
	got, err := m.CPULimitRule()
	if err != nil {
		t.Fatal(err)
	}
	if got != DefaultCPULimitRule() {
		t.Fatalf("unset rule = %+v, want default %+v", got, DefaultCPULimitRule())
	}
	want := CPULimitRule{Enabled: true, WindowMinutes: 3, Percent: 25, CoresX10: 2, DurationSeconds: 3600}
	if err := m.SetCPULimitRule(want); err != nil {
		t.Fatal(err)
	}
	got, err = m.CPULimitRule()
	if err != nil || got != want {
		t.Fatalf("round trip = %+v, err=%v, want %+v", got, err, want)
	}
	if err := m.SetCPULimitRule(CPULimitRule{Enabled: true, WindowMinutes: 0}); err == nil {
		t.Fatal("invalid rule accepted")
	}
}

func TestCPUOverStreak(t *testing.T) {
	const step = 60
	now := int64(6000)
	running := func(min int64, pct int64) db.ResourceSample {
		return db.ResourceSample{SampleMinute: min, State: 1, CPUPercentX10: pct}
	}
	cases := []struct {
		name    string
		samples []db.ResourceSample
		need    int
		thr     int64
		want    bool
	}{
		{"three contiguous over", []db.ResourceSample{running(now-180, 700), running(now-120, 800), running(now-60, 900)}, 3, 600, true},
		{"newest not fresh", []db.ResourceSample{running(now-600, 700), running(now-540, 800), running(now-480, 900)}, 3, 600, false},
		{"gap breaks run", []db.ResourceSample{running(now-180, 700), running(now-120, 800), running(now, 900)}, 3, 600, false},
		{"equal threshold not over", []db.ResourceSample{running(now-120, 600), running(now-60, 600)}, 2, 600, false},
		{"stopped breaks run", []db.ResourceSample{running(now-120, 700), {SampleMinute: now - 60, State: 0, CPUPercentX10: 700}}, 2, 600, false},
		{"need more than present", []db.ResourceSample{running(now-60, 700)}, 3, 600, false},
	}
	for _, tc := range cases {
		if got := cpuOverStreak(tc.samples, now, tc.need, tc.thr); got != tc.want {
			t.Errorf("%s: cpuOverStreak = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestEnforceCPULimitsDisabledNoActive(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "off.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.CreateUser("alice", "h", "10.115.0.2", 1, 30001, 10000, 40, 1024, 10); err != nil {
		t.Fatal(err)
	}
	c := cfg.Default()
	c.Incus.Socket = "/nonexistent/vpsmgr-test.sock"
	m := New(c, d)

	// Rule unset (disabled), nothing active: no Incus call, no error.
	if err := m.EnforceCPULimits(); err != nil {
		t.Fatalf("EnforceCPULimits with rule off: %v", err)
	}
}

func TestEnforceCPULimitsCleansDeletedUser(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "del.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c := cfg.Default()
	c.Incus.Socket = "/nonexistent/vpsmgr-test.sock"
	m := New(c, d)

	active := map[string]CPULimitState{"ghost": {Until: 1 << 40, CoresX10: 5}}
	b, _ := json.Marshal(active)
	if err := d.SetSetting(db.SettingCPULimitActive, string(b)); err != nil {
		t.Fatal(err)
	}
	// The user does not exist, so the entry is dropped without touching Incus.
	if err := m.EnforceCPULimits(); err != nil {
		t.Fatalf("EnforceCPULimits: %v", err)
	}
	if len(m.CPULimits()) != 0 {
		t.Fatalf("active limits = %+v, want empty", m.CPULimits())
	}
}

func TestEnforceCPULimitsRestoreFailurePropagates(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "restore.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	u, err := d.CreateUser("bob", "h", "10.115.0.3", 2, 30002, 10100, 40, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Default()
	c.Incus.Socket = "/nonexistent/vpsmgr-test.sock"
	m := New(c, d)

	active := map[string]CPULimitState{u.Name: {Until: 1, CoresX10: 5}} // already expired
	b, _ := json.Marshal(active)
	if err := d.SetSetting(db.SettingCPULimitActive, string(b)); err != nil {
		t.Fatal(err)
	}
	if err := m.EnforceCPULimits(); err == nil {
		t.Fatal("expected an error: restoring an expired limit must reach Incus")
	}
	// The restore failed, so the entry is kept for the next pass.
	if _, ok := m.CPULimits()[u.Name]; !ok {
		t.Fatal("active entry dropped despite a failed restore")
	}
}
