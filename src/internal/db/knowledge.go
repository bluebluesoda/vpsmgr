package db

import (
	"database/sql"
	"errors"
	"time"
)

// kbNow is a nanosecond timestamp: the article list is ordered by updated_at,
// and editing an older article must move it to the top even within the same
// second.
func kbNow() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// Knowledge is one knowledge-base article. Content is Markdown, rendered by the
// panels; it is stored verbatim.
type Knowledge struct {
	ID        int64
	Title     string
	Content   string
	CreatedAt string
	UpdatedAt string
}

// ListKnowledge returns every article, most recently updated first.
func (d *DB) ListKnowledge() ([]Knowledge, error) {
	rows, err := d.sql.Query(
		`SELECT id, title, content, created_at, updated_at FROM knowledge
		 ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Knowledge
	for rows.Next() {
		var k Knowledge
		if err := rows.Scan(&k.ID, &k.Title, &k.Content, &k.CreatedAt, &k.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// GetKnowledge returns one article. A missing id is (nil, nil).
func (d *DB) GetKnowledge(id int64) (*Knowledge, error) {
	var k Knowledge
	switch err := d.sql.QueryRow(
		`SELECT id, title, content, created_at, updated_at FROM knowledge WHERE id=?`, id).
		Scan(&k.ID, &k.Title, &k.Content, &k.CreatedAt, &k.UpdatedAt); {
	case err == nil:
		return &k, nil
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	default:
		return nil, err
	}
}

// CreateKnowledge inserts a new article and returns its id.
func (d *DB) CreateKnowledge(title, content string) (int64, error) {
	ts := kbNow()
	r, err := d.sql.Exec(
		`INSERT INTO knowledge(title, content, created_at, updated_at) VALUES(?,?,?,?)`,
		title, content, ts, ts)
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

// UpdateKnowledge rewrites an article's title/content and bumps updated_at.
func (d *DB) UpdateKnowledge(id int64, title, content string) error {
	_, err := d.sql.Exec(
		`UPDATE knowledge SET title=?, content=?, updated_at=? WHERE id=?`,
		title, content, kbNow(), id)
	return err
}

// DeleteKnowledge removes an article (no-op when it does not exist).
func (d *DB) DeleteKnowledge(id int64) error {
	_, err := d.sql.Exec(`DELETE FROM knowledge WHERE id=?`, id)
	return err
}
