package mgr

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/lx"
)

// BandwidthInterval is how often the background sampler runs.
const BandwidthInterval = 60 * time.Second

// SampleBandwidth reads the current Incus network counters of every running
// container and advances each user's monthly transfer totals. Counter resets
// (container restart/reinstall) and the monthly rollover (period key change)
// are handled inside the DB update, so concurrent samplers (background
// goroutine and CLI) can never double-count: the delta is computed against the
// baselines stored in the database at statement time, not against values read
// in this process.
func (m *Manager) SampleBandwidth() error {
	m.sampleMu.Lock()
	defer m.sampleMu.Unlock()
	tm, err := m.lx.BandwidthMap()
	if err != nil {
		return err
	}
	return m.sampleBandwidth(tm)
}

func (m *Manager) sampleBandwidth(tm map[string]lx.Bandwidth) error {
	users, err := m.db.ListUsers()
	if err != nil {
		return err
	}
	if len(users) == 0 {
		return nil
	}
	period := time.Now().UTC().Format("2006-01")
	var firstErr error
	for _, u := range users {
		t, ok := tm[u.Name]
		if !ok {
			// container stopped: no counters available, nothing to add
			continue
		}
		if err := m.db.ApplyBandwidth(u.ID, period, t.Rx, t.Tx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
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
