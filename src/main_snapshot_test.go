package main

import (
	"database/sql"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
	"vpsmgr/internal/mgr"
)

func TestParseSnapshotLimitFlag(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		provided  bool
		want      int
		invalid   bool
	}{
		{"configured default", "", false, 0, false},
		{"omitted preserves quota", "12", false, 0, false},
		{"explicit zero", "0", true, 0, false},
		{"explicit limit", "7", true, 7, false},
		{"trimmed limit", " 3 ", true, 3, false},
		{"blank", "", true, 0, true},
		{"negative", "-1", true, 0, true},
		{"malformed", "bad", true, 0, true},
		{"fraction", "1.5", true, 0, true},
		{"overflow", "999999999999999999999999", true, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limit, err := parseSnapshotLimitFlag(tc.raw, tc.provided)
			if (err != nil) != tc.invalid {
				t.Fatalf("limit=%v error=%v, invalid=%v", limit, err, tc.invalid)
			}
			if tc.invalid {
				return
			}
			if (limit == nil) == tc.provided {
				t.Fatalf("limit=%v, provided=%v", limit, tc.provided)
			}
			if limit != nil && *limit != tc.want {
				t.Fatalf("limit=%d, want %d", *limit, tc.want)
			}
		})
	}
}

func TestSnapshotCLIValidationBeforeConfig(t *testing.T) {
	// A directory cannot be loaded as a config. A snapshot validation error
	// therefore proves neither command reached config loading or DB migration.
	t.Setenv("VPSMGR_CONFIG", t.TempDir())
	for _, command := range []struct {
		name string
		run  func([]string) error
	}{{"add", userAdd}, {"quota", userQuota}} {
		for _, value := range []string{"", "-1", "bad", "1.5", "999999999999999999999999"} {
			t.Run(command.name+"/"+value, func(t *testing.T) {
				err := command.run([]string{"alice", "--snapshots=" + value})
				if err == nil || !strings.Contains(err.Error(), "--snapshots must be a nonnegative integer") {
					t.Fatalf("error=%v", err)
				}
			})
		}
		for _, value := range []string{"0", "7"} {
			t.Run(command.name+"/valid-"+value, func(t *testing.T) {
				err := command.run([]string{"alice", "--snapshots=" + value, "--days=-1"})
				if err == nil || !strings.Contains(err.Error(), "--days must be") {
					t.Fatalf("valid snapshot flag rejected or validation reached config: %v", err)
				}
			})
		}
	}
}

func TestQuotaSnapshotMixedFlagsValidateBeforeConfig(t *testing.T) {
	t.Setenv("VPSMGR_CONFIG", t.TempDir())
	for _, tc := range []struct {
		flag, value, want string
	}{
		{"cpu", "bad", "cpu"},
		{"mem", "bad", "memory"},
		{"disk", "bad", "disk"},
		{"bandwidth", "-1", "bandwidth"},
		{"days", "0", "--days must be positive"},
	} {
		t.Run(tc.flag, func(t *testing.T) {
			err := userQuota([]string{"alice", "--snapshots=0", "--" + tc.flag + "=" + tc.value})
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("error=%v, want %q before config load", err, tc.want)
			}
		})
	}
}

func snapshotCLIConfig(t *testing.T) *cfg.Config {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("VPSMGR_CONFIG", filepath.Join(dir, "config.yaml"))
	c := cfg.Default()
	c.Net.ExtIF = "test0"
	c.Panel.PublicIP = "192.0.2.1"
	c.Panel.DisplayIP = "192.0.2.1"
	c.Panel.DB = filepath.Join(dir, "panel.db")
	c.Incus.Socket = "/nonexistent/vpsmgr-test.sock"
	if err := cfg.Save(c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConfigSnapshotDefaultMigratesLegacyQuotaFirst(t *testing.T) {
	c := snapshotCLIConfig(t)
	c.Snapshots.Limit = 7
	if err := cfg.Save(c); err != nil {
		t.Fatal(err)
	}
	d, err := db.OpenWithSnapshotLimit(c.Panel.DB, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.CreateUser("alice", "h", "192.0.2.2", 1, 30001, 10000, 10, 1024, 10); err != nil {
		d.Close()
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	// Re-create the schema21 state before per-user quotas existed.
	legacy, err := sql.Open("sqlite", c.Panel.DB)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`ALTER TABLE users DROP COLUMN snapshot_limit`,
		`DELETE FROM schema_migrations WHERE version=22`,
	} {
		if _, err := legacy.Exec(statement); err != nil {
			legacy.Close()
			t.Fatal(err)
		}
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	if err := configSet([]string{"snapshots.limit", "0", "--no-apply"}); err != nil {
		t.Fatal(err)
	}
	freshConfig, err := cfg.Load()
	if err != nil || freshConfig.Snapshots.Limit != 0 {
		t.Fatalf("configured default=%+v error=%v", freshConfig, err)
	}
	// Opening with the NEW default must not rewrite the pre-existing user.
	d, err = db.OpenWithSnapshotLimit(c.Panel.DB, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	u, err := d.GetUserByName("alice")
	if err != nil || u.SnapshotLimit != 7 {
		t.Fatalf("legacy user=%+v error=%v; want quota 7", u, err)
	}
}

func TestConfigSnapshotDefaultMissingOrBrokenDatabase(t *testing.T) {
	for _, broken := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "broken"}[broken], func(t *testing.T) {
			c := snapshotCLIConfig(t)
			c.Snapshots.Limit = 7
			if err := cfg.Save(c); err != nil {
				t.Fatal(err)
			}
			if broken {
				if err := os.WriteFile(c.Panel.DB, []byte("not a sqlite database"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			err := configSet([]string{"snapshots.limit", "0", "--no-apply"})
			if (err != nil) != broken {
				t.Fatalf("broken=%v error=%v", broken, err)
			}
			fresh, err := cfg.Load()
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if broken {
				want = 7
			}
			if fresh.Snapshots.Limit != want {
				t.Fatalf("default=%d want %d", fresh.Snapshots.Limit, want)
			}
			if !broken {
				if _, err := os.Stat(c.Panel.DB); !os.IsNotExist(err) {
					t.Fatalf("configuration edit created a database: %v", err)
				}
			}
		})
	}
}

func TestSnapshotCLIUsageAndOutput(t *testing.T) {
	snapshotCLIConfig(t)
	capture := func(fn func()) string {
		t.Helper()
		f, err := os.Create(filepath.Join(t.TempDir(), "stdout"))
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		old := os.Stdout
		os.Stdout = f
		defer func() { os.Stdout = old }()
		fn()
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(f)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	help := capture(usage)
	for _, want := range []string{"[--snapshots N]", "0 disables new snapshots", "use snapshots.limit", "preserve the user's limit"} {
		if !strings.Contains(help, want) {
			t.Errorf("help missing %q", want)
		}
	}
	for _, print := range []func(*mgr.Result){printAdded, printResult} {
		for _, limit := range []int{0, 4} {
			r := &mgr.Result{User: &db.User{Name: "alice", CPU: 10, MemMB: 1024, DiskGB: 10, SnapshotLimit: limit}}
			out := capture(func() { print(r) })
			want := "snapshots 0 (0 = no new snapshots)"
			if limit == 4 {
				want = "snapshots 4 (0 = no new snapshots)"
			}
			if !strings.Contains(out, want) {
				t.Errorf("output missing %q: %s", want, out)
			}
		}
	}
}
