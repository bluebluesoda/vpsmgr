package db

func (d *DB) UpdateUsersPasswordAndDeleteSessions(ids []int64, passHash, keepToken string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE users SET pass_hash=? WHERE id=?`, passHash, id); err != nil {
			return err
		}
		if keepToken == "" {
			if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id=?`, id); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(`DELETE FROM sessions WHERE user_id=? AND token<>?`, id, hashToken(keepToken)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) UpdateUsersColor(ids []int64, color string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.Exec(`UPDATE users SET color=? WHERE id=?`, color, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
