package db

// AdminKey is one public key the operator manages from the admin panel. Same
// conventions as SSHKey (clean "type base64", name label, active flag) but not
// bound to a user — it is the admin's own key store.
type AdminKey struct {
	ID        int64
	Name      string
	Key       string
	Active    bool
	CreatedAt string
}

func (d *DB) AddAdminKey(name, key string, active bool) (*AdminKey, error) {
	k := &AdminKey{Name: name, Key: key, Active: active, CreatedAt: now()}
	r, err := d.sql.Exec(
		`INSERT INTO admin_keys(name, key, active, created_at) VALUES(?,?,?,?)`,
		k.Name, k.Key, b2i(k.Active), k.CreatedAt)
	if err != nil {
		return nil, err
	}
	k.ID, _ = r.LastInsertId()
	return k, nil
}

func (d *DB) UpdateAdminKey(id int64, name, key string, active bool) error {
	_, err := d.sql.Exec(`UPDATE admin_keys SET name=?, key=?, active=? WHERE id=?`,
		name, key, b2i(active), id)
	return err
}

func (d *DB) DeleteAdminKey(id int64) error {
	_, err := d.sql.Exec(`DELETE FROM admin_keys WHERE id=?`, id)
	return err
}

func (d *DB) ListAdminKeys() ([]AdminKey, error) {
	rows, err := d.sql.Query(`SELECT id, name, key, active, created_at FROM admin_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminKey
	for rows.Next() {
		k := AdminKey{}
		if err := rows.Scan(&k.ID, &k.Name, &k.Key, &k.Active, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// GrantedAdminKeys returns the operator keys this user has activated, joined
// from the grants table so the live admin key content is always used (a later
// rename/delete of an admin key is reflected here).
func (d *DB) GrantedAdminKeys(userID int64) ([]AdminKey, error) {
	rows, err := d.sql.Query(`
		SELECT a.id, a.name, a.key, a.active, a.created_at
		FROM admin_key_grants g
		JOIN admin_keys a ON a.id = g.admin_key_id
		WHERE g.user_id = ? ORDER BY a.id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdminKey
	for rows.Next() {
		k := AdminKey{}
		if err := rows.Scan(&k.ID, &k.Name, &k.Key, &k.Active, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// SetAdminKeyGrants replaces the user's grant set wholesale: all previous
// grants are dropped and the given admin-key IDs become the new set.
func (d *DB) SetAdminKeyGrants(userID int64, ids []int64) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM admin_key_grants WHERE user_id=?`, userID); err != nil {
		return err
	}
	ts := now()
	for _, id := range ids {
		// OR IGNORE is a backstop against a duplicate id sneaking through;
		// the caller already dedupes.
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO admin_key_grants(user_id, admin_key_id, created_at) VALUES(?,?,?)`,
			userID, id, ts); err != nil {
			return err
		}
	}
	return tx.Commit()
}