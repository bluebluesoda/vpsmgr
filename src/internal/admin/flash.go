package admin

import (
	"sync"
	"time"
)

type flashEntry struct {
	msg  string
	kind string
	ts   time.Time
}

// flashStore holds one-shot banners per admin session token in memory. Same
// pattern as the user panel: banners are transient and never leak into URLs.
type flashStore struct {
	mu    sync.Mutex
	items map[string]*flashEntry
}

func newFlashStore() *flashStore { return &flashStore{items: make(map[string]*flashEntry)} }

const (
	flashTTL      = 10 * time.Minute
	flashMaxItems = 2048
)

func (f *flashStore) Set(token, msg, kind string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.items) >= flashMaxItems {
		now := time.Now()
		for k, e := range f.items {
			if now.Sub(e.ts) > flashTTL {
				delete(f.items, k)
			}
		}
	}
	f.items[token] = &flashEntry{msg: msg, kind: kind, ts: time.Now()}
}

func (f *flashStore) Pop(token string) (msg, kind string, ok bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.items[token]
	if !ok {
		return "", "toast", false
	}
	delete(f.items, token)
	return e.msg, e.kind, true
}

func (f *flashStore) Clear(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.items, token)
}
