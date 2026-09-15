package mgr

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"vpsmgr/internal/cfg"
	"vpsmgr/internal/db"
)

// CPULimitRule is the global dynamic CPU limit rule, configured under
// `cpu_limit.*` in config.yaml (`vps config set cpu_limit.…`). It applies to
// every container: when a container keeps its CPU usage above Percent of its
// own quota for WindowMinutes in a row, it is capped to CoresX10 tenths of a
// core (the same time-slice semantics as a fractional quota) for
// DurationSeconds, then its normal quota is restored.
type CPULimitRule struct {
	Enabled         bool
	WindowMinutes   int // consecutive minutes over the threshold
	Percent         int // percent of the container's own quota, 1..100
	CoresX10        int // limit target, 1..10 tenths (0.1..1.0 core)
	DurationSeconds int // how long the limit lasts once applied
}

// CPULimitState is the persisted state of one container's active dynamic limit.
type CPULimitState struct {
	Until    int64 `json:"until"`     // unix seconds the limit expires
	CoresX10 int   `json:"cores_x10"` // the cap that was applied
}

// ValidateCPULimitRule rejects out-of-range rule parameters, so the enforcement
// loop only ever acts on a sane rule (the config registry guarantees it on set;
// this is the belt-and-braces check for a hand-edited config file).
func ValidateCPULimitRule(r CPULimitRule) error {
	if r.WindowMinutes < 1 {
		return errors.New("window must be at least 1 minute")
	}
	if r.Percent < 1 || r.Percent > 100 {
		return errors.New("percent must be between 1 and 100")
	}
	if r.CoresX10 < 1 || r.CoresX10 > 10 {
		return errors.New("cores must be between 0.1 and 1")
	}
	if r.Enabled && r.DurationSeconds <= 0 {
		return errors.New("duration must be greater than 0 when the rule is enabled")
	}
	if r.DurationSeconds < 0 {
		return errors.New("duration must not be negative")
	}
	return nil
}

// CPULimitRuleFromConfig derives the rule from the config file (`cpu_limit.*`).
func CPULimitRuleFromConfig(c *cfg.Config) CPULimitRule {
	cl := c.CPULimit
	return CPULimitRule{
		Enabled:         cl.Enabled,
		WindowMinutes:   cl.WindowMinutes,
		Percent:         cl.Percent,
		CoresX10:        int(math.Round(cl.Cores * 10)),
		DurationSeconds: cl.DurationHours*3600 + cl.DurationMinutes*60,
	}
}

// CPULimitRule returns the live rule: the DB mirror written by `vps config set`
// (and `vps install`), falling back to the config file when the mirror is
// unset. The config file stays authoritative; the mirror is what lets the
// long-running panel see a change without a restart (same pattern as
// net.v4_forward).
func (m *Manager) CPULimitRule() CPULimitRule {
	if r, ok, err := m.mirroredCPULimitRule(); err == nil && ok {
		return r
	}
	return CPULimitRuleFromConfig(m.cfg)
}

// SetCPULimitRule writes the mirror so the running panel applies the rule
// immediately. Called by `vps config set cpu_limit.*` and by `vps install`.
func (m *Manager) SetCPULimitRule(r CPULimitRule) error {
	if err := ValidateCPULimitRule(r); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return m.db.SetSetting(db.SettingCPULimitRule, string(b))
}

func (m *Manager) mirroredCPULimitRule() (CPULimitRule, bool, error) {
	v, ok, err := m.db.GetSetting(db.SettingCPULimitRule)
	if err != nil || !ok || v == "" {
		return CPULimitRule{}, false, err
	}
	var r CPULimitRule
	if err := json.Unmarshal([]byte(v), &r); err != nil {
		return CPULimitRule{}, false, err
	}
	return r, true, nil
}

// CPULimits returns the containers currently carrying a dynamic CPU limit,
// keyed by container name. Safe to call from panel goroutines.
func (m *Manager) CPULimits() map[string]CPULimitState {
	active, err := m.loadCPULimits()
	if err != nil {
		return map[string]CPULimitState{}
	}
	return active
}

func (m *Manager) loadCPULimits() (map[string]CPULimitState, error) {
	v, ok, err := m.db.GetSetting(db.SettingCPULimitActive)
	if err != nil {
		return nil, err
	}
	if !ok || v == "" {
		return map[string]CPULimitState{}, nil
	}
	var active map[string]CPULimitState
	if err := json.Unmarshal([]byte(v), &active); err != nil {
		return nil, fmt.Errorf("parse active cpu limits: %w", err)
	}
	if active == nil {
		active = map[string]CPULimitState{}
	}
	return active, nil
}

func (m *Manager) saveCPULimits(active map[string]CPULimitState) error {
	if active == nil {
		active = map[string]CPULimitState{}
	}
	b, err := json.Marshal(active)
	if err != nil {
		return err
	}
	return m.db.SetSetting(db.SettingCPULimitActive, string(b))
}

// EnforceCPULimits applies and expires the dynamic CPU limit for every
// container. It restores the normal quota of containers whose limit expired,
// whose user was deleted, or while the rule is disabled/invalid, then caps any
// container that has been over Percent of its quota for WindowMinutes straight.
// Called by the 60s sampler (single goroutine). Restoring uses the user's
// current DB quota (users.cpu), so a quota edit during a limit wins.
func (m *Manager) EnforceCPULimits() error {
	m.cpuLimitMu.Lock()
	defer m.cpuLimitMu.Unlock()

	rule := m.CPULimitRule()
	// A hand-edited config can carry an out-of-range rule: treat it as disabled
	// (never trigger; restore anything active) instead of acting on it.
	if err := ValidateCPULimitRule(rule); err != nil {
		rule.Enabled = false
	}
	active, err := m.loadCPULimits()
	if err != nil {
		return err
	}
	users, err := m.db.ListUsers()
	if err != nil {
		return err
	}
	byName := make(map[string]*db.User, len(users))
	for _, u := range users {
		byName[u.Name] = u
	}

	now := time.Now().Unix()
	step := int64(ResourceSampleInterval / time.Second)
	var firstErr error
	changed := false

	// Restore/forget limits that expired, belong to a deleted user, or are
	// orphaned by the rule being switched off.
	for name, st := range active {
		u := byName[name]
		if u == nil {
			delete(active, name)
			changed = true
			continue
		}
		if !rule.Enabled || now >= st.Until {
			if err := m.lx.SetCPU(name, u.CPU); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("restore cpu quota for %s: %w", name, err)
				}
				continue
			}
			delete(active, name)
			changed = true
		}
	}

	// Apply the limit to containers that stayed over the threshold.
	if rule.Enabled {
		since := now - int64(rule.WindowMinutes+2)*step
		samples, err := m.db.RecentResourceSamples(since)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			grouped := make(map[int64][]db.ResourceSample)
			for _, s := range samples {
				grouped[s.UserID] = append(grouped[s.UserID], s)
			}
			threshold := int64(rule.Percent) * 10
			for _, u := range users {
				if _, ok := active[u.Name]; ok {
					continue
				}
				// A limit at or above the container's own quota would raise it
				// instead of restricting it, so skip those.
				if u.CPU <= rule.CoresX10 {
					continue
				}
				if !cpuOverStreak(grouped[u.ID], now, rule.WindowMinutes, threshold) {
					continue
				}
				if err := m.lx.SetCPU(u.Name, rule.CoresX10); err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("apply cpu limit to %s: %w", u.Name, err)
					}
					continue
				}
				active[u.Name] = CPULimitState{Until: now + int64(rule.DurationSeconds), CoresX10: rule.CoresX10}
				changed = true
			}
		}
	}

	if changed {
		if err := m.saveCPULimits(active); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// cpuOverStreak reports whether the user's newest `need` samples form a
// contiguous run of running minutes, each above thresholdX10 (tenths of a
// percent of the container's own quota). The newest sample must be recent, so
// a sampling gap (panel down) never counts as sustained load.
func cpuOverStreak(samples []db.ResourceSample, now int64, need int, thresholdX10 int64) bool {
	if need < 1 || len(samples) < need {
		return false
	}
	last := samples[len(samples)-1]
	if now-last.SampleMinute > 120 {
		return false
	}
	step := int64(ResourceSampleInterval / time.Second)
	expected := last.SampleMinute
	count := 0
	for i := len(samples) - 1; i >= 0; i-- {
		s := samples[i]
		if s.SampleMinute != expected {
			break
		}
		if s.State != resourceRunning || s.CPUPercentX10 <= thresholdX10 {
			break
		}
		count++
		if count >= need {
			return true
		}
		expected -= step
	}
	return false
}
