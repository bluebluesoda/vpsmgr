package cfg

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ImmutableFields are the config keys fixed after the first install. Changing
// any of them would break existing containers/wiring, so their values are
// snapshotted into the DB on the first `vps install` and verified on every
// later `vps install` / `vps serve` — "shouldn't be changed" is enforced, not
// just documented. Note: panel.url_path is here (moving the user entrance
// strands users) but panel.admin_url_path is NOT — the admin path is
// operator-editable and can be emptied to disable the admin panel.
var ImmutableFields = []string{
	"net.subnet",
	"net.gateway",
	"incus.pool",
	"incus.bridge",
	"panel.url_path",
}

// ImmutableSnapshot renders the immutable fields as JSON for storage in the DB
// settings table.
func (c *Config) ImmutableSnapshot() (string, error) {
	m := make(map[string]string, len(ImmutableFields))
	for _, k := range ImmutableFields {
		m[k] = FieldValue(c, k)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("immutable snapshot: %w", err)
	}
	return string(b), nil
}

// VerifyImmutable compares the live config against a stored snapshot and
// reports which immutable fields drifted. Fields missing from an old snapshot
// are ignored so a pre-snapshot install is not blocked on new keys.
func (c *Config) VerifyImmutable(snapshot string) error {
	var m map[string]string
	if err := json.Unmarshal([]byte(snapshot), &m); err != nil {
		return fmt.Errorf("invalid immutable snapshot: %w", err)
	}
	// Pre-incus-rename snapshots used lxd.pool / lxd.bridge; treat them as the
	// same fields so an upgrade is verified against the historical baseline
	// instead of silently skipping the check. A legacy snapshot where these
	// are EMPTY means they were never snapshotted (the pre-fix code always
	// wrote "") — treat that as missing and skip, so an existing install is
	// not falsely flagged as drifted on upgrade.
	if v, ok := m["lxd.pool"]; ok {
		if v != "" {
			m["incus.pool"] = v
		}
	}
	if v, ok := m["lxd.bridge"]; ok {
		if v != "" {
			m["incus.bridge"] = v
		}
	}
	var drifted []string
	for _, k := range ImmutableFields {
		want, ok := m[k]
		if !ok {
			continue
		}
		if FieldValue(c, k) != want {
			drifted = append(drifted, k)
		}
	}
	if len(drifted) > 0 {
		return fmt.Errorf("immutable config changed since install (%s) — restore these in %s or reinstall; changing them breaks existing containers",
			strings.Join(drifted, ", "), Path())
	}
	return nil
}
