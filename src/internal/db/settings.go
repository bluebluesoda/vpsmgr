package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Settings keys. The settings table is the DB's key/value store for panel
// state that used to live in the config file but must not be hand-edited:
// credentials and install-time invariants.
const (
	// SettingAdminPassHash is the bcrypt hash of the admin panel password
	// (no admin username). Moved here from config.yaml so the credential is
	// panel-managed state, not an editable file field.
	SettingAdminPassHash = "admin_pass_hash"

	// SettingImmutableSnapshot is a JSON snapshot of the install-time-fixed
	// config fields (net.subnet, lxd.pool, panel.url_path, ...), written once
	// on the first `vps install`. `vps install`/`vps serve` refuse to run when
	// the live config has drifted from it, so "shouldn't be changed" fields
	// are enforced rather than just documented.
	SettingImmutableSnapshot = "immutable_snapshot"

	// SettingV4Forward mirrors net.v4_forward in the DB so the long-running
	// panel process sees a toggle made by `vps config set net.v4_forward`
	// immediately (its in-memory config is only loaded at startup). Written by
	// mgr.ApplyV4State; the panel reads it live to decide whether domains may
	// be added.
	SettingV4Forward = "v4_forward"

	// SettingTraefik mirrors net.traefik so the long-running panel sees a
	// runtime toggle without a panel restart.
	SettingTraefik = "traefik"

	// SettingBlockedDomains is the admin blocked-domains list, stored as
	// newline-separated normalized domains. A blocked domain and all its
	// subdomains are refused by AddDomain (admin-managed via the web UI).
	SettingBlockedDomains = "blocked_domains"

	// SettingCPULimitRule is the admin-managed global dynamic CPU limit rule
	// (JSON), applied to every container. Absent = the rule is disabled.
	SettingCPULimitRule = "cpu_limit_rule"

	// SettingCPULimitActive is the JSON map of containers currently under a
	// dynamic CPU limit (name -> {until, cores_x10}). Persisted so the limit
	// survives a panel restart and the countdown stays accurate.
	SettingCPULimitActive = "cpu_limit_active"

	// SettingSnapshotShareEnabled toggles the snapshot-sharing feature (publish
	// a checkpoint behind a code, install from someone's checkpoint). Absent
	// means ENABLED, so a host upgraded to this version gets the feature on by
	// default; the admin can turn it off. Disabling hides the user-side UI and
	// refuses new shares/installs, but the stored codes are deliberately kept
	// (not deleted) so re-enabling restores them while their checkpoint still
	// exists. Stored as "1"/"0".
	SettingSnapshotShareEnabled = "snapshot_share_enabled"
)

// GetSetting returns a settings value; ok is false when the key is absent.
func (d *DB) GetSetting(key string) (value string, ok bool, err error) {
	var v string
	switch err := d.sql.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v); {
	case err == nil:
		return v, true, nil
	case errors.Is(err, sql.ErrNoRows):
		return "", false, nil
	default:
		return "", false, fmt.Errorf("db: get setting %s: %w", key, err)
	}
}

// SetSetting upserts a settings value.
func (d *DB) SetSetting(key, value string) error {
	if _, err := d.sql.Exec(
		`INSERT INTO settings(key, value) VALUES(?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value); err != nil {
		return fmt.Errorf("db: set setting %s: %w", key, err)
	}
	return nil
}

// GetBlockedDomains returns the admin blocked-domains list (already
// normalized, no empty entries). An absent or empty key yields an empty list.
func (d *DB) GetBlockedDomains() ([]string, error) {
	v, ok, err := d.GetSetting(SettingBlockedDomains)
	if err != nil {
		return nil, err
	}
	if !ok || v == "" {
		return nil, nil
	}
	return strings.Split(v, "\n"), nil
}

// SetBlockedDomains persists the blocked-domains list, one domain per line.
func (d *DB) SetBlockedDomains(list []string) error {
	return d.SetSetting(SettingBlockedDomains, strings.Join(list, "\n"))
}

// SnapshotShareEnabled reports whether snapshot sharing is on. An absent key
// means enabled — the feature's default after an upgrade.
func (d *DB) SnapshotShareEnabled() (bool, error) {
	v, ok, err := d.GetSetting(SettingSnapshotShareEnabled)
	if err != nil {
		return false, err
	}
	if !ok || v == "" {
		return true, nil
	}
	return v != "0" && v != "false", nil
}

// SetSnapshotShareEnabled turns snapshot sharing on or off (stored as "1"/"0").
func (d *DB) SetSnapshotShareEnabled(on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	return d.SetSetting(SettingSnapshotShareEnabled, v)
}
