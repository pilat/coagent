package progressruntime

import (
	"context"
	"sync"

	"github.com/pilat/coagent/internal/progress"
	"github.com/pilat/coagent/internal/sessionstore"
)

var _ Service = (*runtime)(nil)

// Service projects durable status together with the current runner context.
type Service interface {
	Current(ctx context.Context, rootID int64) (string, error)
	SetLive(sessionID int64, live Live)
}

type runtime struct {
	sessionStore *sessionstore.Store
	mu           sync.RWMutex
	live         map[int64]Live
}

// Live carries only the process-local part of the status projection.
type Live struct {
	Active  bool
	Context progress.Context
}

func New(store *sessionstore.Store) Service {
	return &runtime{sessionStore: store, live: make(map[int64]Live)}
}

func (r *runtime) Current(ctx context.Context, rootID int64) (string, error) {
	return r.current(ctx, rootID)
}

func (r *runtime) SetLive(sessionID int64, live Live) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.live[sessionID] = live
}

func (r *runtime) liveState(sessionID int64) Live {
	r.mu.RLock()
	defer r.mu.RUnlock()

	return r.live[sessionID]
}
