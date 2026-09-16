package mgr

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"vpsmgr/internal/cfg"
)

// BandwidthInterval is how often the background resource sampler runs.
const BandwidthInterval = 60 * time.Second

// SampleBandwidth is retained as a compatibility name for callers that request
// a sample. Resource sampling now records all metrics and bandwidth together.
func (m *Manager) SampleBandwidth() error {
	return m.SampleResources()
}

// BandwidthFor returns the user's monthly upload/download totals in bytes.
func (m *Manager) BandwidthFor(userID int64) (up, down uint64) {
	tr, err := m.db.GetBandwidth(userID)
	if err != nil {
		return 0, 0
	}
	return tr.Upload, tr.Download
}

// FormatGB renders bytes as GB with one decimal place (e.g. 12.3).
func FormatGB(bytes uint64) string {
	return fmt.Sprintf("%.1f", float64(bytes)/(1<<30))
}

// shouldThrottle reports whether a user with quotaGB GiB (0 = unlimited) has
// exceeded it given used bytes (upload + download this month).
func shouldThrottle(used uint64, quotaGB int) bool {
	if quotaGB <= 0 {
		return false
	}
	return used >= uint64(quotaGB)<<30
}

// BandwidthPeriod returns the monthly bandwidth period key ("YYYY-MM") for the
// given time and configured reset day. The period starts on day resetDay, so
// before that day the key is the previous month's (which is what rolls the
// counters over). resetDay is clamped to 1..28 — 28 keeps February safe — and
// any out-of-range value falls back to 1.
func BandwidthPeriod(t time.Time, resetDay int) string {
	if resetDay < 1 || resetDay > 28 {
		resetDay = 1
	}
	t = t.UTC()
	if t.Day() >= resetDay {
		return t.Format("2006-01")
	}
	firstOfMonth := time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
	return firstOfMonth.AddDate(0, -1, 0).Format("2006-01")
}

// BandwidthNextReset returns the moment the quota period in force ends, i.e.
// when the current period's counters roll over.
//
// It is the configured day of the month AFTER the current period's start
// month — not "the configured day next month". The distinction matters right
// after the reset day has passed: with resetDay=1 and today mid-month, the next
// reset is the 1st of the FOLLOWING month, while with resetDay=22 and today the
// 16th it is the 22nd of THIS month (the period still started last month).
//
// The caller renders it; ThisNextMonth reports whether that lands in the
// current calendar month or the next one. resetDay is clamped exactly like
// BandwidthPeriod (1..28) so the two can never disagree about the boundary.
func BandwidthNextReset(t time.Time, resetDay int) time.Time {
	if resetDay < 1 || resetDay > 28 {
		resetDay = 1
	}
	t = t.UTC()
	// Same period arithmetic as BandwidthPeriod: on/after resetDay we are in
	// this month's period, before it we are still in last month's.
	year, month := t.Year(), t.Month()
	if t.Day() < resetDay {
		month--
		if month < time.January {
			month = time.December
			year--
		}
	}
	// The period ends on the reset day of the following month.
	firstOfPeriod := time.Date(year, month, 1, 0, 0, 0, 0, time.UTC)
	end := firstOfPeriod.AddDate(0, 1, 0)
	return time.Date(end.Year(), end.Month(), resetDay, 0, 0, 0, 0, time.UTC)
}

// ParseBandwidthGB parses a bandwidth quota in GiB: empty or "0" = unlimited,
// otherwise a non-negative integer.
func ParseBandwidthGB(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, errors.New("bandwidth quota must be a non-negative integer GiB (0 = unlimited)")
	}
	return n, nil
}

// IsThrottled reports whether the container currently carries the bandwidth
// throttle. Safe to call from panel goroutines (limitMu-guarded).
func (m *Manager) IsThrottled(name string) bool {
	m.limitMu.Lock()
	defer m.limitMu.Unlock()
	return m.throttled[name]
}

// ResetBandwidth zeroes a user's monthly transfer totals. If the user was
// currently throttled for exceeding the quota, the NIC limit is removed
// immediately (not just on the next 60s sampler pass) and the in-memory
// throttle state is cleared, so an over-quota user is un-throttled right away.
func (m *Manager) ResetBandwidth(name string) error {
	u, err := m.db.GetUserByName(name)
	if err != nil {
		return err
	}
	if err := m.db.ResetBandwidth(u.ID); err != nil {
		return err
	}
	m.limitMu.Lock()
	defer m.limitMu.Unlock()
	if m.throttled[u.Name] {
		if err := m.lx.EnsureNicRateLimit(u.Name, ""); err != nil {
			return fmt.Errorf("unthrottle %s after bandwidth reset: %w", u.Name, err)
		}
		delete(m.throttled, u.Name)
	}
	return nil
}

// EnforceBandwidthLimits applies or removes the NIC rate limit for every user
// based on their monthly quota. Called by the 60s sampler only (single
// goroutine), so two containers crossing the limit in the same pass are both
// handled without racing. Incus applies NIC limits live via tc (no container
// restart), and the throttled map makes the call idempotent between passes.
func (m *Manager) EnforceBandwidthLimits() error {
	m.limitMu.Lock()
	defer m.limitMu.Unlock()
	users, err := m.db.ListUsers()
	if err != nil {
		return err
	}
	// First pass after a process restart: prime the throttle map from the
	// limits actually present on the containers, not from memory. Otherwise a
	// container that was throttled before the restart and is now back under
	// quota (e.g. monthly rollover) would never have its stale NIC limit
	// removed.
	if m.throttled == nil {
		m.throttled = map[string]bool{}
		for _, u := range users {
			if rate, err := m.lx.NicRateLimit(u.Name); err == nil && rate != "" {
				m.throttled[u.Name] = true
			}
		}
	}
	var firstErr error
	for _, u := range users {
		up, down := m.BandwidthFor(u.ID)
		over := shouldThrottle(up+down, u.BandwidthQuotaGB)
		if over && !m.throttled[u.Name] {
			if err := m.lx.EnsureNicRateLimit(u.Name, cfg.ThrottleRate); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("throttle %s: %w", u.Name, err)
				}
				continue
			}
			m.throttled[u.Name] = true
		} else if !over && m.throttled[u.Name] {
			if err := m.lx.EnsureNicRateLimit(u.Name, ""); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("unthrottle %s: %w", u.Name, err)
				}
				continue
			}
			delete(m.throttled, u.Name)
		}
	}
	return firstErr
}
