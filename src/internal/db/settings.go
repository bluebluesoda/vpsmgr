package db

import (
	"database/sql"
	"errors"
	"fmt"
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
