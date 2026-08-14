package cfg

import (
	"strings"
	"testing"
)

func TestImmutableSnapshotRoundTrip(t *testing.T) {
	c := Default()
	c.Net.Subnet = "10.42.0.0/24"
	snap, err := c.ImmutableSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snap, `"net.subnet":"10.42.0.0/24"`) {
		t.Fatalf("snapshot missing net.subnet: %s", snap)
	}
	if err := c.VerifyImmutable(snap); err != nil {
		t.Fatalf("verify against own snapshot: %v", err)
	}
}

func TestVerifyImmutableDetectsDrift(t *testing.T) {
	c := Default()
	snap, err := c.ImmutableSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	c.Net.Subnet = "10.9.0.0/24"
	err = c.VerifyImmutable(snap)
	if err == nil {
		t.Fatal("drifted net.subnet accepted")
	}
	if !strings.Contains(err.Error(), "net.subnet") {
		t.Fatalf("error should name the drifted field: %v", err)
	}
}

func TestVerifyImmutableIgnoresOldSnapshotKeys(t *testing.T) {
	c := Default()
	// A snapshot from before panel.url_path existed: missing keys must not
	// block a config that has them.
	err := c.VerifyImmutable(`{"net.subnet":"10.115.0.0/24"}`)
	if err != nil {
		t.Fatalf("old snapshot missing new keys blocked: %v", err)
	}
}

func TestVerifyImmutableBadSnapshot(t *testing.T) {
	c := Default()
	if err := c.VerifyImmutable("not-json"); err == nil {
		t.Fatal("corrupt snapshot accepted")
	}
}

func TestImmutableSnapshotIncludesIncusFields(t *testing.T) {
	c := Default()
	c.Incus.Pool = "vpsmgr"
	c.Incus.Bridge = "incusbr0"
	snap, err := c.ImmutableSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"incus.pool", "incus.bridge"} {
		if !strings.Contains(snap, `"`+k+`"`) {
			t.Fatalf("snapshot missing %s: %s", k, snap)
		}
	}
}

func TestVerifyImmutableDetectsIncusDrift(t *testing.T) {
	c := Default()
	c.Incus.Pool = "vpsmgr"
	c.Incus.Bridge = "incusbr0"
	snap, err := c.ImmutableSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	// Drifting the pool must be caught (it was silently ignored before the
	// lxd.* → incus.* fix).
	c.Incus.Pool = "otherpool"
	err = c.VerifyImmutable(snap)
	if err == nil {
		t.Fatal("drifted incus.pool accepted")
	}
	if !strings.Contains(err.Error(), "incus.pool") {
		t.Fatalf("error should name the drifted field: %v", err)
	}
}

func TestVerifyImmutableMigratesLegacyLxdKeys(t *testing.T) {
	c := Default()
	c.Incus.Pool = "vpsmgr"
	c.Incus.Bridge = "incusbr0"
	// A snapshot written by a pre-incus build (lxd.pool / lxd.bridge): the
	// same physical values under the old key names must pass.
	legacy := `{"lxd.pool":"vpsmgr","lxd.bridge":"incusbr0","net.subnet":"10.115.0.0/24"}`
	if err := c.VerifyImmutable(legacy); err != nil {
		t.Fatalf("legacy lxd.* snapshot blocked: %v", err)
	}
	// ...and drift under the legacy key must be caught too.
	c.Incus.Bridge = "otherbr"
	err := c.VerifyImmutable(legacy)
	if err == nil {
		t.Fatal("drifted legacy lxd.bridge accepted")
	}
}

// TestVerifyImmutableSkipsEmptyLegacyPool covers real installations: the
// pre-fix snapshot code ALWAYS wrote lxd.pool/lxd.bridge as "" (the registry
// had no such fields), so a real legacy snapshot carries empty values. Those
// must be treated as "never snapshotted" — skipped — or every existing install
// would be flagged as drifted on upgrade.
func TestVerifyImmutableSkipsEmptyLegacyPool(t *testing.T) {
	c := Default()
	c.Incus.Pool = "vpsmgr"
	c.Incus.Bridge = "incusbr0"
	legacy := `{"lxd.pool":"","lxd.bridge":"","net.subnet":"10.115.0.0/24"}`
	if err := c.VerifyImmutable(legacy); err != nil {
		t.Fatalf("empty legacy lxd.* snapshot blocked: %v", err)
	}
}
