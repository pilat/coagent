package ctl

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrAlreadyRunning means another daemon holds the lock. A second daemon must
// exit rather than compete: two processes on one SQLite file under WAL corrupt
// each other silently, and two socket owners make "which daemon answered?"
// unanswerable.
var ErrAlreadyRunning = errors.New("another coagent daemon is already running")

// Lock is the single-instance guard. It is held for the daemon's whole life;
// only its holder may remove a stale socket and bind.
type Lock struct {
	file *os.File
}

// Acquire takes an exclusive, non-blocking flock on path. The lock is advisory
// and released by the kernel when the process dies, so a crashed daemon never
// leaves a lock nobody can clear.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock dir: %w", err)
	}

	// Go opens with O_CLOEXEC, so the restart exec drops this lock and the new
	// image re-acquires it. A racing daemon still loses on the flock.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()

		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}

		return nil, fmt.Errorf("lock %s: %w", path, err)
	}

	return &Lock{file: f}, nil
}

// Release drops the lock. The file itself stays: recreating it on every boot
// would race a second daemon that already opened the old inode.
func (l *Lock) Release() error {
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
