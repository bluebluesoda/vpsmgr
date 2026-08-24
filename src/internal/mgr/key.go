package mgr

import (
	"fmt"
	"regexp"
	"strings"

	"vpsmgr/internal/db"
)

// sshKeyRe matches an OpenSSH public key: `type base64 [comment]`. The body is
// greedy over the base64 alphabet and stops at the first non-base64 character,
// so any trailing comment (spaces, @, etc.) lands in the optional group. A
// comment made only of base64 characters would be folded into the body — the
// same tradeoff the old client-side parser made, and harmless: the stored key
// stays valid either way.
var sshKeyRe = regexp.MustCompile(`^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp256|ecdsa-sha2-nistp384|ecdsa-sha2-nistp521|sk-ssh-ed25519|sk-ecdsa-sha2-nistp256|ssh-dss)\s+([A-Za-z0-9+/=]{32,})\s*(.*)$`)

// ParsePublicKey validates and normalizes a public key. It returns the clean
// key ("type base64", trailing comment stripped) plus the comment ("" when
// none). ok is false for anything that is not a well-formed key.
func ParsePublicKey(s string) (clean, comment string, ok bool) {
	m := sshKeyRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", "", false
	}
	return m[1] + " " + m[2], strings.TrimSpace(m[3]), true
}

// AdminKeyMarker is appended as a trailing comment to an operator key line when
// it is written into a user's ~/.ssh/authorized_keys. SSH ignores anything
// after the key body, so it never affects login; it exists so the user can
// recognize and find admin-injected keys on their machine even after the
// operator deletes the key from the panel (the DB grant is gone, but the
// authorized_keys line remains). It is a plain ASCII token — never localized —
// so the file stays clean and re-applies deduplicate reliably.
const AdminKeyMarker = "from-admin"

// SSHKeyInput is one key row submitted by the panel. ID > 0 updates an
// existing key; ID == 0 adds a new one.
type SSHKeyInput struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Key    string `json:"key"`
	Active bool   `json:"active"`
}

// SaveSSHKeys reconciles the user's key set from the panel: rows with an ID
// update their existing record, rows without one are added, and existing keys
// missing from the submission are deleted. Every key is re-validated and
// stored clean (comment stripped); an empty name falls back to the key's
// comment, then "key". Returns the fresh full list. Pure DB work — the caller
// applies the active keys to the container.
func (m *Manager) SaveSSHKeys(name string, in []SSHKeyInput) ([]db.SSHKey, error) {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return nil, err
	}
	existing, err := m.db.ListSSHKeys(u.ID)
	if err != nil {
		return nil, err
	}
	// Clean keys already present (kept ones plus this save's new ones), to
	// reject a duplicate rather than silently storing the same key twice.
	seen := map[string]bool{}
	kept := map[int64]bool{}
	added := []SSHKeyInput{}
	for _, k := range in {
		clean, comment, ok := ParsePublicKey(k.Key)
		if !ok {
			return nil, fmt.Errorf("not a valid SSH public key")
		}
		if seen[clean] {
			return nil, fmt.Errorf("this SSH key is already added")
		}
		seen[clean] = true
		nm := strings.TrimSpace(k.Name)
		if nm == "" {
			nm = comment
		}
		if nm == "" {
			nm = "key"
		}
		if k.ID > 0 {
			kept[k.ID] = true
			if err := m.db.UpdateSSHKey(k.ID, nm, clean, k.Active); err != nil {
				return nil, err
			}
		} else {
			added = append(added, SSHKeyInput{Name: nm, Key: clean, Active: k.Active})
		}
	}
	// Existing keys the panel no longer sent are deleted.
	for _, e := range existing {
		if !kept[e.ID] {
			if err := m.db.DeleteSSHKey(e.ID); err != nil {
				return nil, err
			}
		}
	}
	for _, k := range added {
		if _, err := m.db.AddSSHKey(u.ID, k.Name, k.Key, k.Active); err != nil {
			return nil, err
		}
	}
	return m.db.ListSSHKeys(u.ID)
}

// ActiveKeys returns the clean key strings currently selected for injection.
func (m *Manager) ActiveKeys(name string) ([]string, error) {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return nil, err
	}
	keys, err := m.db.ActiveSSHKeys(u.ID)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k.Key)
	}
	return out, nil
}

// SaveAdminKeys reconciles the operator's own key store from the admin panel:
// same semantics as SaveSSHKeys (ID updates, ID 0 adds, missing rows deleted,
// clean storage, duplicate rejection) against the admin_keys table. Every key
// the operator keeps is active (visible to every user) — there is no per-key
// "active" toggle in the admin panel, so the flag is always stored true. Keys
// are not injected into any container by themselves; a user opts in per key.
func (m *Manager) SaveAdminKeys(in []SSHKeyInput) ([]db.AdminKey, error) {
	existing, err := m.db.ListAdminKeys()
	if err != nil {
		return nil, err
	}
	type pending struct{ name, key string; active bool }
	seen := map[string]bool{}
	kept := map[int64]bool{}
	var toAdd []pending
	for _, k := range in {
		clean, comment, ok := ParsePublicKey(k.Key)
		if !ok {
			return nil, fmt.Errorf("not a valid SSH public key")
		}
		if seen[clean] {
			return nil, fmt.Errorf("this SSH key is already added")
		}
		seen[clean] = true
		nm := strings.TrimSpace(k.Name)
		if nm == "" {
			nm = comment
		}
		if nm == "" {
			nm = "key"
		}
		if k.ID > 0 {
			kept[k.ID] = true
			if err := m.db.UpdateAdminKey(k.ID, nm, clean, true); err != nil {
				return nil, err
			}
		} else {
			toAdd = append(toAdd, pending{nm, clean, true})
		}
	}
	for _, e := range existing {
		if !kept[e.ID] {
			if err := m.db.DeleteAdminKey(e.ID); err != nil {
				return nil, err
			}
		}
	}
	for _, k := range toAdd {
		if _, err := m.db.AddAdminKey(k.name, k.key, k.active); err != nil {
			return nil, err
		}
	}
	return m.db.ListAdminKeys()
}

// ListAdminKeys returns the operator's stored keys.
func (m *Manager) ListAdminKeys() ([]db.AdminKey, error) {
	return m.db.ListAdminKeys()
}

// SaveAdminKeyGrants reconciles which of the operator's keys this user has
// authorized (checked in the SSH-key panel). Only IDs of keys that actually
// exist are kept — a stale or unknown ID is dropped, never granted. Returns
// the resulting live grant list (joined key contents) so the panel can
// re-render and the caller can apply.
func (m *Manager) SaveAdminKeyGrants(name string, ids []int64) ([]db.AdminKey, error) {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return nil, err
	}
	valid := map[int64]bool{}
	if keys, err := m.db.ListAdminKeys(); err != nil {
		return nil, err
	} else {
		for _, k := range keys {
			valid[k.ID] = true
		}
	}
	seen := map[int64]bool{}
	var clean []int64
	for _, id := range ids {
		if id > 0 && valid[id] && !seen[id] {
			seen[id] = true
			clean = append(clean, id)
		}
	}
	if err := m.db.SetAdminKeyGrants(u.ID, clean); err != nil {
		return nil, err
	}
	return m.db.GrantedAdminKeys(u.ID)
}

// GrantedAdminKeys returns the operator keys this user has activated, with
// their live contents (for syncing into the container's authorized_keys).
func (m *Manager) GrantedAdminKeys(name string) ([]db.AdminKey, error) {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return nil, err
	}
	return m.db.GrantedAdminKeys(u.ID)
}

// ApplySSHKeys syncs the container's ~/.ssh/authorized_keys. The two kinds of
// keys behave differently, deliberately:
//
//   - adminKeys (the operator keys this user activated) are FULLY SYNCED. Every
//     line carrying the AdminKeyMarker is dropped, then the activated keys are
//     re-appended with the marker. So an admin key the user unchecks — or one
//     the operator has since deleted — is removed from the machine, while an
//     activated one is present exactly once. Stale admin lines never linger.
//   - userKeys (the user's own active panel keys) are APPEND-only: added if
//     missing, never removed. A key the user added by hand and never put in the
//     panel is untouched, because there is no marker to identify it.
//
// The script embeds each key in single quotes; ParsePublicKey guarantees the
// stored keys contain only the type name, a space and base64 characters, and
// AdminKeyMarker is a fixed ASCII token, so no quoting can be escaped.
func (m *Manager) ApplySSHKeys(name string, userKeys []string, adminKeys []db.AdminKey) error {
	_, err := m.lx.ExecSH(name, applySSHKeysScript(userKeys, adminKeys))
	return err
}

// applySSHKeysScript renders the shell script that syncs authorized_keys. It
// is a pure string builder so the sync logic can be unit-tested without Incus.
func applySSHKeysScript(userKeys []string, adminKeys []db.AdminKey) string {
	var b strings.Builder
	b.WriteString("set -e\n")
	b.WriteString("mkdir -p /root/.ssh\n")
	b.WriteString("chmod 700 /root/.ssh\n")
	b.WriteString("AUTH=/root/.ssh/authorized_keys\n")
	b.WriteString("touch \"$AUTH\"\n")
	b.WriteString("chmod 600 \"$AUTH\"\n")
	// Drop every admin-managed line (those ending with the marker) first, then
	// re-add the ones that are still activated. This makes admin keys a clean
	// sync: unactivated/deleted ones are removed, activated ones present once.
	b.WriteString("sed -i '/ " + AdminKeyMarker + "$/d' \"$AUTH\"\n")
	for _, k := range adminKeys {
		line := k.Key + " " + AdminKeyMarker
		b.WriteString("grep -qxF -- '")
		b.WriteString(line)
		b.WriteString("' \"$AUTH\" 2>/dev/null || printf '%s\\n' '")
		b.WriteString(line)
		b.WriteString("' >> \"$AUTH\"\n")
	}
	// The user's own active keys are append-only (added if missing, never
	// removed).
	for _, k := range userKeys {
		b.WriteString("grep -qxF -- '")
		b.WriteString(k)
		b.WriteString("' \"$AUTH\" 2>/dev/null || printf '%s\\n' '")
		b.WriteString(k)
		b.WriteString("' >> \"$AUTH\"\n")
	}
	return b.String()
}