package admin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"vpsmgr/internal/mgr"
)

// batchJobTTL bounds how long a finished batch stays readable. The one-time
// passwords live only in this struct — nothing is persisted — exactly like the
// credentials the single-create flash modal carries.
const batchJobTTL = 30 * time.Minute

// batchItem is one user's state inside a batch.
type batchItem struct {
	Name     string `json:"name"`
	State    string `json:"state"`
	Password string `json:"password,omitempty"`
	Error    string `json:"error,omitempty"`
	Warning  string `json:"warning,omitempty"`
}

// batchJob is an in-memory batch-create run. A panel restart loses the progress
// and the passwords, but every container already created stays usable.
type batchJob struct {
	ID       string      `json:"id"`
	Total    int         `json:"total"`
	Done     int         `json:"done"`
	Finished bool        `json:"finished"`
	Items    []batchItem `json:"items"`

	createdAt time.Time
}

// batchJobs is the process-wide batch registry. Only one batch runs at a time:
// creations are serialized by the manager anyway, and two interleaved batches
// would make the progress view meaningless.
type batchJobs struct {
	mu      sync.Mutex
	jobs    map[string]*batchJob
	running string
}

func newBatchJobs() *batchJobs { return &batchJobs{jobs: map[string]*batchJob{}} }

// start registers a new batch and refuses while another one is still running.
func (b *batchJobs) start(names []string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pruneLocked()
	if b.running != "" {
		return "", errors.New("a batch create is already running")
	}
	id, err := newJobID()
	if err != nil {
		return "", err
	}
	j := &batchJob{ID: id, Total: len(names), Items: make([]batchItem, 0, len(names)), createdAt: time.Now()}
	for _, n := range names {
		j.Items = append(j.Items, batchItem{Name: n, State: "pending"})
	}
	b.jobs[id] = j
	b.running = id
	return id, nil
}

// update applies one progress report to the item it names.
func (b *batchJobs) update(id string, r mgr.BatchResult) {
	b.mu.Lock()
	defer b.mu.Unlock()
	j, ok := b.jobs[id]
	if !ok {
		return
	}
	for i := range j.Items {
		if j.Items[i].Name != r.Name {
			continue
		}
		j.Items[i].State = r.State
		if r.Password != "" {
			j.Items[i].Password = r.Password
		}
		if r.Err != nil {
			j.Items[i].Error = r.Err.Error()
		}
		if r.Warning != "" {
			j.Items[i].Warning = r.Warning
		}
		if r.State == mgr.BatchDone || r.State == mgr.BatchFailed {
			j.Done++
		}
		return
	}
}

// finish marks a batch complete and releases the running slot.
func (b *batchJobs) finish(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	j, ok := b.jobs[id]
	if !ok {
		return
	}
	j.Finished = true
	if b.running == id {
		b.running = ""
	}
}

// get returns a copy, so JSON encoding never races the writer goroutine.
func (b *batchJobs) get(id string) (batchJob, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	j, ok := b.jobs[id]
	if !ok {
		return batchJob{}, false
	}
	cp := *j
	cp.Items = append([]batchItem(nil), j.Items...)
	return cp, true
}

// pruneLocked drops expired jobs. A running batch is never pruned: fifty clones
// can legitimately outlive the TTL. Caller holds the lock.
func (b *batchJobs) pruneLocked() {
	for id, j := range b.jobs {
		if id == b.running {
			continue
		}
		if time.Since(j.createdAt) > batchJobTTL {
			delete(b.jobs, id)
		}
	}
}

func newJobID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
