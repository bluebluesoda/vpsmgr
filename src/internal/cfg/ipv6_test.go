package cfg

import (
	"strings"
	"testing"
)

func TestIPv6PoolValidated(t *testing.T) {
	c := &Config{Net: NetCfg{IPv6Pool: []string{
		"2001:db8:1::9c4",
		"2001:db8:1::9c5",
	}}}
	got, err := c.IPv6PoolValidated()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "2001:db8:1::9c4" {
		t.Errorf("validated pool = %v", got)
	}

	// Prefix length rejected.
	bad := []string{"2001:db8:1::9c4/64"}
	if _, err := (&Config{Net: NetCfg{IPv6Pool: bad}}).IPv6PoolValidated(); err == nil || !strings.Contains(err.Error(), "prefix") {
		t.Errorf("expected prefix-length rejection, got %v", err)
	}
	// ULA rejected.
	bad = []string{"fc00::1"}
	if _, err := (&Config{Net: NetCfg{IPv6Pool: bad}}).IPv6PoolValidated(); err == nil {
		t.Error("expected ULA rejection")
	}
	// Link-local rejected.
	bad = []string{"fe80::1"}
	if _, err := (&Config{Net: NetCfg{IPv6Pool: bad}}).IPv6PoolValidated(); err == nil {
		t.Error("expected link-local rejection")
	}
	// IPv4 rejected.
	bad = []string{"1.2.3.4"}
	if _, err := (&Config{Net: NetCfg{IPv6Pool: bad}}).IPv6PoolValidated(); err == nil {
		t.Error("expected IPv4 rejection")
	}
	// Duplicate rejected.
	bad = []string{"2001:db8::1", "2001:db8::1"}
	if _, err := (&Config{Net: NetCfg{IPv6Pool: bad}}).IPv6PoolValidated(); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate rejection, got %v", err)
	}
	// Empty pool is valid (nil, no error).
	if got, err := (&Config{Net: NetCfg{IPv6Pool: nil}}).IPv6PoolValidated(); err != nil || got != nil {
		t.Errorf("empty pool: got %v err %v, want nil,nil", got, err)
	}
}

func TestIPv6ModeEffective(t *testing.T) {
	// Empty mode + pool set -> pool.
	c := &Config{Net: NetCfg{IPv6Pool: []string{"2001:db8::1"}}}
	if got := c.IPv6ModeEffective(); got != IPv6ModePool {
		t.Errorf("pool-only config mode = %q, want pool", got)
	}
	// Empty mode + subnet set -> prefix (legacy config).
	c = &Config{Net: NetCfg{IPv6Subnet: "2602:fada:6::/64"}}
	if got := c.IPv6ModeEffective(); got != IPv6ModePrefix {
		t.Errorf("subnet-only config mode = %q, want prefix", got)
	}
	// Nothing -> none.
	c = &Config{}
	if got := c.IPv6ModeEffective(); got != IPv6ModeNone {
		t.Errorf("empty config mode = %q, want none", got)
	}
	// Explicit mode wins.
	c = &Config{Net: NetCfg{IPv6Mode: IPv6ModePool, IPv6Subnet: "2602:fada:6::/64"}}
	if got := c.IPv6ModeEffective(); got != IPv6ModePool {
		t.Errorf("explicit pool mode = %q, want pool", got)
	}
}
