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
