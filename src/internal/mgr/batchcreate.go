package mgr

import (
	"errors"
	"fmt"
	"strings"
)

// MaxBatchUsers caps a single batch create. Every user costs a container, a
// user-port block and pool space, so a pasted list of a few thousand lines must
// not be able to churn the host for hours by accident.
const MaxBatchUsers = 50

// Batch item states, reported by AddBatch as each user is worked on.
const (
	BatchRunning = "running"
	BatchDone    = "done"
	BatchFailed  = "failed"
)

// BatchResult is one user's progress in a batch create. Password is empty when
// the panel password was inherited from an existing group member.
type BatchResult struct {
	Name     string
	State    string
	Password string
	Err      error
	Warning  string
}

// ValidateBatchNames normalizes a pasted list and checks the whole thing before
// anything is created: syntax, duplicates within the list, and names that
// already exist. It returns the cleaned names, so the caller creates exactly
// what was validated. Blank lines are treated as padding, not errors.
func (m *Manager) ValidateBatchNames(names []string) ([]string, error) {
	var (
		out   []string
		seen  = map[string]bool{}
		probs []string
	)
	for i, raw := range names {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		if len(out) >= MaxBatchUsers {
			probs = append(probs, fmt.Sprintf("more than %d users in one batch", MaxBatchUsers))
			break
		}
		// Same rule as a single create from the admin panel: child names are
		// allowed there and the panel cannot tell them apart up front.
		if err := m.ValidateAddName(name, true); err != nil {
			probs = append(probs, fmt.Sprintf("line %d (%s): %v", i+1, name, err))
			continue
		}
		if seen[name] {
			probs = append(probs, fmt.Sprintf("line %d: %s is listed twice", i+1, name))
			continue
		}
		if _, err := m.db.GetUserByName(name); err == nil {
			probs = append(probs, fmt.Sprintf("line %d: user already exists: %s", i+1, name))
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	if len(probs) > 0 {
		// Keep the message short enough for a toast: show the first few, count
		// the rest.
		const show = 5
		tail := ""
		if len(probs) > show {
			tail = fmt.Sprintf("; and %d more", len(probs)-show)
			probs = probs[:show]
		}
		return nil, errors.New(strings.Join(probs, "; ") + tail)
	}
	if len(out) == 0 {
		return nil, errors.New("no usernames given")
	}
	return out, nil
}

// AddBatch creates each name in turn. It is meant to run on a background
// goroutine: report is called for every state change so the caller can track
// progress and collect the one-time passwords. Users are independent — one
// failure does not stop the rest of the batch.
func (m *Manager) AddBatch(names []string, opt AddOptions, adminKeyIDs []int64, report func(BatchResult)) {
	for _, name := range names {
		report(BatchResult{Name: name, State: BatchRunning})
		res, err := m.Add(name, opt)
		if err != nil {
			report(BatchResult{Name: name, State: BatchFailed, Err: err})
			continue
		}
		r := BatchResult{Name: name, State: BatchDone, Password: res.Password}
		if len(adminKeyIDs) > 0 {
			r.Warning = m.grantAdminKeys(name, adminKeyIDs)
		}
		report(r)
	}
}

// grantAdminKeys activates the operator's keys for a freshly created user and
// writes them into the container. It returns a message rather than an error:
// the container is already usable and the user can still tick the keys in the
// panel afterwards, so this must not fail the create.
func (m *Manager) grantAdminKeys(name string, ids []int64) string {
	granted, err := m.SaveAdminKeyGrants(name, ids)
	if err != nil {
		return "grant admin keys: " + err.Error()
	}
	if err := m.ApplySSHKeys(name, nil, granted); err != nil {
		return "apply admin keys: " + err.Error()
	}
	return ""
}
