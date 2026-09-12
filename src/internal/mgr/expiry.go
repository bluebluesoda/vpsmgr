package mgr

import (
	"fmt"
	"time"

	"vpsmgr/internal/db"
)

// expiryLayout is the RFC3339 UTC format stored in users.expires_at.
const expiryLayout = time.RFC3339

// IsExpired reports whether an expiry timestamp (RFC3339 UTC, "" = permanent)
// has passed. Pure.
func IsExpired(expiresAt string, now time.Time) bool {
	if expiresAt == "" {
		return false
	}
	t, err := time.Parse(expiryLayout, expiresAt)
	if err != nil {
		return false
	}
	return now.After(t)
}

// ExpiryRemaining returns the time left until the deadline, or 0 for a
// permanent/unparseable value. Negative once past.
func ExpiryRemaining(expiresAt string, now time.Time) time.Duration {
	if expiresAt == "" {
		return 0
	}
	t, err := time.Parse(expiryLayout, expiresAt)
	if err != nil {
		return 0
	}
	return t.Sub(now)
}

// ExpiryFromDays converts a validity in days to an absolute RFC3339 UTC
// timestamp (0 or negative = permanent, i.e. "").
func ExpiryFromDays(days int) string {
	if days <= 0 {
		return ""
	}
	return time.Now().UTC().AddDate(0, 0, days).Format(expiryLayout)
}

// ExtendExpiry computes a new deadline by adding d to the later of now and the
// current deadline, so an already-expired account gets the full duration. A
// permanent account (empty current) gets a fresh deadline of now + d.
func ExtendExpiry(current string, now time.Time, d time.Duration) string {
	base := now
	if t, err := time.Parse(expiryLayout, current); err == nil && t.After(now) {
		base = t
	}
	return base.Add(d).UTC().Format(expiryLayout)
}

// SetExpiry stores an absolute deadline ("" = permanent) and returns it.
func (m *Manager) SetExpiry(name, expiresAt string) (string, error) {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return "", err
	}
	if err := m.db.UpdateUserExpiry(u.ID, expiresAt); err != nil {
		return "", err
	}
	return expiresAt, nil
}

// ExtendExpiryFor extends a user's deadline by d (max(now, current) + d) and
// returns the new deadline.
func (m *Manager) ExtendExpiryFor(name string, d time.Duration) (string, error) {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return "", err
	}
	next := ExtendExpiry(u.ExpiresAt, time.Now().UTC(), d)
	if err := m.db.UpdateUserExpiry(u.ID, next); err != nil {
		return "", err
	}
	return next, nil
}

// EnforceQuotaExpiry force-stops every container whose quota validity has
// passed. Called by the 60s sampler only (single goroutine). It acts only while
// a container is not stopped, so it is idempotent across passes: a container
// already down is left alone, and a manual start is re-stopped on the next
// pass. Autostart is disabled so the container cannot come back on its own.
func (m *Manager) EnforceQuotaExpiry() error {
	users, err := m.db.ListUsers()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	var firstErr error
	for _, u := range users {
		if u.Status != db.StatusReady || !IsExpired(u.ExpiresAt, now) {
			continue
		}
		st, err := m.lx.State(u.Name)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("expiry check %s: %w", u.Name, err)
			}
			continue
		}
		if st == "Stopped" {
			continue
		}
		if err := m.lx.SetAutostart(u.Name, false); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("expiry disable autostart %s: %w", u.Name, err)
			}
			continue
		}
		if err := m.lx.ForceStop(u.Name); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("expiry force stop %s: %w", u.Name, err)
			}
			continue
		}
		_ = m.db.AddAuditLog("000", "quota.expire."+u.Name)
	}
	return firstErr
}
