package db

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
)

// LockOperations serializes container operations across daemon and CLI processes.
// The sidecar is never removed during normal use. Do not flock the database
// itself: closing an extra FD on that inode can release SQLite's POSIX locks.
// Root-created locks inherit the DB owner so the unprivileged daemon can use
// them too. The caller must not nest acquisitions and must always call unlock.
func (d *DB) LockOperations() (unlock func(), err error) {
	info, err := os.Stat(d.path)
	if err != nil {
		return nil, fmt.Errorf("stat database for operation lock: %w", err)
	}
	path := d.path + ".operations.lock"
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open database operation lock: %w", err)
	}
	f := os.NewFile(uintptr(fd), path)
	if os.Geteuid() == 0 {
		owner := info.Sys().(*syscall.Stat_t)
		if err := f.Chown(int(owner.Uid), int(owner.Gid)); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("set operation lock owner: %w", err)
		}
	}
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			break
		}
	}
	if err != nil {
		closeErr := f.Close()
		return nil, fmt.Errorf("lock database operations: %w", errors.Join(err, closeErr))
	}
	var once sync.Once
	return func() {
		once.Do(func() { _ = f.Close() })
	}, nil
}
