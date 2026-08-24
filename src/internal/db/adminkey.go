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