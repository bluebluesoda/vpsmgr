package db

import "database/sql"

// SSHKey is one public key the user manages from the panel. Key is always the
// clean "type base64" form (any trailing comment stripped); Name is the human
// label (defaults to the key's comment when the user does not type one).
// Active selects which keys are written into ~/.ssh/authorized_keys.
type SSHKey struct {
	ID        int64
	UserID    int64
	Name      string
	Key       string
	Active    bool
	CreatedAt string
}

func (d *DB) AddSSHKey(userID int64, name, key string, active bool) (*SSHKey, error) {
	k := &SSHKey{UserID: userID, Name: name, Key: key, Active: active, CreatedAt: now()}
	r, err := d.sql.Exec(
		`INSERT INTO ssh_keys(user_id, name, key, active, created_at) VALUES(?,?,?,?,?)`,
		k.UserID, k.Name, k.Key, b2i(k.Active), k.CreatedAt)
	if err != nil {
		return nil, err
	}
	k.ID, _ = r.LastInsertId()
	return k, nil
}

func (d *DB) UpdateSSHKey(id int64, name, key string, active bool) error {
	_, err := d.sql.Exec(`UPDATE ssh_keys SET name=?, key=?, active=? WHERE id=?`,
		name, key, b2i(active), id)
	return err
}

func (d *DB) DeleteSSHKey(id int64) error {
	_, err := d.sql.Exec(`DELETE FROM ssh_keys WHERE id=?`, id)
	return err
}

func scanSSHKey(row *sql.Row) (*SSHKey, error) {
	k := &SSHKey{}
	err := row.Scan(&k.ID, &k.UserID, &k.Name, &k.Key, &k.Active, &k.CreatedAt)
	if err != nil {
		return nil, err
	}
	return k, nil
}

func (d *DB) ListSSHKeys(userID int64) ([]SSHKey, error) {
	rows, err := d.sql.Query(
		`SELECT id, user_id, name, key, active, created_at FROM ssh_keys WHERE user_id=? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SSHKey
	for rows.Next() {
		k := SSHKey{}
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.Key, &k.Active, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// ActiveSSHKeys returns the keys currently selected for injection, for a fresh
// container (reinstall) or a live apply.
func (d *DB) ActiveSSHKeys(userID int64) ([]SSHKey, error) {
	rows, err := d.sql.Query(
		`SELECT id, user_id, name, key, active, created_at FROM ssh_keys WHERE user_id=? AND active=1 ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SSHKey
	for rows.Next() {
		k := SSHKey{}
		if err := rows.Scan(&k.ID, &k.UserID, &k.Name, &k.Key, &k.Active, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
