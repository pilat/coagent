package builtin

import (
	"path/filepath"
	"sync"
)

// fileLocks is process-wide on purpose: a parent and its subagents share the
// workspace, so a per-stack lock would not cover them.
var fileLocks = &pathLocks{entries: make(map[string]*pathLockEntry)}

type pathLocks struct {
	mu      sync.Mutex
	entries map[string]*pathLockEntry
}

type pathLockEntry struct {
	rw   sync.RWMutex
	refs int
}

// lockFileWrite excludes every other tool from path for the whole
// read-modify-write cycle and returns the release func. Tool calls of one
// assistant response run concurrently; two unsynchronized whole-file rewrites
// drop one edit and leave the longer content's tail past the shorter one.
func lockFileWrite(path string) func() {
	entry, key := fileLocks.acquire(path)
	entry.rw.Lock()

	return func() {
		entry.rw.Unlock()
		fileLocks.release(key)
	}
}

// lockFileRead blocks while a mutation holds path; concurrent reads proceed.
func lockFileRead(path string) func() {
	entry, key := fileLocks.acquire(path)
	entry.rw.RLock()

	return func() {
		entry.rw.RUnlock()
		fileLocks.release(key)
	}
}

func (p *pathLocks) acquire(path string) (*pathLockEntry, string) {
	key := filepath.Clean(path)

	p.mu.Lock()
	defer p.mu.Unlock()

	entry, ok := p.entries[key]
	if !ok {
		entry = &pathLockEntry{}
		p.entries[key] = entry
	}

	entry.refs++

	return entry, key
}

func (p *pathLocks) release(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	entry, ok := p.entries[key]
	if !ok {
		return
	}

	entry.refs--

	if entry.refs == 0 {
		delete(p.entries, key)
	}
}
