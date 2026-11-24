package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/pilat/coagent/internal/coagenthome"
)

// Two daemons would share both the SQLite WAL and socket ownership.
var errAlreadyRunning = errors.New("another coagent daemon is already running")

// Only the instanceLock holder may remove stale sockets and bind.
type instanceLock struct {
	file *os.File
}

func acquireInstanceLock() (*instanceLock, error) {
	path, err := coagenthome.Join(coagenthome.LockFileName)
	if err != nil {
		return nil, fmt.Errorf("lock path: %w", err)
	}

	lock, err := acquire(path)
	if err != nil {
		return nil, fmt.Errorf("single-instance lock: %w", err)
	}

	return lock, nil
}

// The kernel releases this advisory lock on death, so crashes cannot strand it.
func acquire(path string) (*instanceLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}

	// Go opens with O_CLOEXEC: restart exec drops the lock, and the new image
	// re-acquires it. A racing daemon still loses on the flock.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()

		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errAlreadyRunning
		}

		return nil, fmt.Errorf("lock %s: %w", path, err)
	}

	return &instanceLock{file: f}, nil
}

// The file stays: recreating it would race another daemon holding the old inode.
func (l *instanceLock) release() error {
	if l == nil || l.file == nil {
		return nil
	}

	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	if err := l.file.Close(); err != nil {
		return fmt.Errorf("close lock file: %w", err)
	}

	l.file = nil

	return nil
}
