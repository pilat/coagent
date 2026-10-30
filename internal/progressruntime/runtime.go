package progressruntime

import (
	"sync"

	"github.com/pilat/coagent/internal/progress"
	"github.com/pilat/coagent/internal/sessionstore"
)

// Runtime projects durable status together with the current runner context.
type Runtime struct {
	sessionStore *sessionstore.Store
	mu           sync.RWMutex
	live         map[int64]Live
}

// Live carries only the process-local part of the status projection.
type Live struct {
	Active  bool
	Context progress.Context
}

func New(store *sessionstore.Store) *Runtime {
	return &Runtime{sessionStore: store, live: make(map[int64]Live)}
}

func (r *Runtime) SetLive(sessionID int64, live Live) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.live[sessionID] = live
}

func (r *Runtime) liveState(sessionID int64) Live {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.live[sessionID]
}
