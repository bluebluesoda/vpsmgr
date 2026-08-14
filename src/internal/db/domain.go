package db

import (
	"database/sql"
	"errors"
)

type Domain struct {
	ID            int64
	UserID        int64
	Domain        string
	ProxyProtocol bool
	CreatedAt     string
	UpdatedAt     string
}

// DomainView is a domain joined with its owning user (admin panel + config
// reconciliation). Time values are UTC (RFC3339).
type DomainView struct {
	ID            int64
	UserID        int64
	Username      string
	IP            string
	Domain        string
	ProxyProtocol bool
	CreatedAt     string
	UpdatedAt     string
}

func (d *DB) AddDomain(userID int64, domain string, proxyProtocol bool) (*Domain, error) {
	ts := now()
	dmn := &Domain{UserID: userID, Domain: domain, ProxyProtocol: proxyProtocol, CreatedAt: ts, UpdatedAt: ts}
	r, err := d.sql.Exec(`INSERT INTO domains(user_id, domain, proxy_protocol, created_at, updated_at) VALUES(?,?,?,?,?)`,
		userID, domain, b2i(proxyProtocol), ts, ts)
	if err != nil {
		return nil, err
	}
	dmn.ID, _ = r.LastInsertId()
	return dmn, nil
}

// GetDomain returns one domain of a user, for update/rollback.
func (d *DB) GetDomain(userID int64, domain string) (*Domain, error) {
	dmn := &Domain{}
	err := d.sql.QueryRow(
		`SELECT id, user_id, domain, proxy_protocol, created_at, updated_at
		 FROM domains WHERE user_id=? AND domain=?`, userID, domain).
		Scan(&dmn.ID, &dmn.UserID, &dmn.Domain, &dmn.ProxyProtocol, &dmn.CreatedAt, &dmn.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return dmn, nil
}

// GetDomainByDomain returns a domain by its (globally unique) name.
func (d *DB) GetDomainByDomain(domain string) (*Domain, error) {
	dmn := &Domain{}
	err := d.sql.QueryRow(
		`SELECT id, user_id, domain, proxy_protocol, created_at, updated_at
		 FROM domains WHERE domain=?`, domain).
		Scan(&dmn.ID, &dmn.UserID, &dmn.Domain, &dmn.ProxyProtocol, &dmn.CreatedAt, &dmn.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return dmn, nil
}

// SetDomainProtocol updates a domain's PROXY protocol flag and bumps
// updated_at. Used both for the real change and the rollback on a failed
// traefik write.
func (d *DB) SetDomainProtocol(id int64, on bool) error {
	_, err := d.sql.Exec(`UPDATE domains SET proxy_protocol=?, updated_at=? WHERE id=?`, b2i(on), now(), id)
	return err
}

func (d *DB) ListDomains(userID int64) ([]*Domain, error) {
	rows, err := d.sql.Query(
		`SELECT id, user_id, domain, proxy_protocol, created_at, updated_at
		 FROM domains WHERE user_id=? ORDER BY id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Domain
	for rows.Next() {
		x := &Domain{}
		if err := rows.Scan(&x.ID, &x.UserID, &x.Domain, &x.ProxyProtocol, &x.CreatedAt, &x.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ListAllDomains returns every domain with its owner's username and container
// IP, newest modification first. Used by the admin domain panel and the
// reconciliation that regenerates the traefik dynamic files from the DB.
func (d *DB) ListAllDomains() ([]*DomainView, error) {
	rows, err := d.sql.Query(
		`SELECT d.id, d.user_id, u.name, u.ip, d.domain, d.proxy_protocol, d.created_at, d.updated_at
		 FROM domains d JOIN users u ON u.id = d.user_id
		 ORDER BY d.updated_at DESC, d.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DomainView
	for rows.Next() {
		x := &DomainView{}
		if err := rows.Scan(&x.ID, &x.UserID, &x.Username, &x.IP, &x.Domain, &x.ProxyProtocol, &x.CreatedAt, &x.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (d *DB) DeleteDomain(userID int64, domain string) error {
	r, err := d.sql.Exec(`DELETE FROM domains WHERE user_id=? AND domain=?`, userID, domain)
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return errors.New("domain not found")
	}
	return nil
}

func (d *DB) DomainExists(domain string) (bool, error) {
	var one int
	err := d.sql.QueryRow(`SELECT 1 FROM domains WHERE domain=?`, domain).Scan(&one)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
