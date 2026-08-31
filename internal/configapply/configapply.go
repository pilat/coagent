package configapply

import (
	"fmt"
	"sync"

	"github.com/pilat/coagent/internal/configops"
)

// Service serializes guarded config commits and their restart handoff.
type Service interface {
	Ops() configops.Service
	PendingCall(sessionID int64) (configops.Pending, error)
	ClaimApply() bool
	ReleaseApply()
	Apply(staged *configops.Staged, pending configops.Pending) configops.Verdict
	Restart()
}

var _ Service = (*svc)(nil)

type svc struct {
	ops     configops.Service
	restart func()

	mu      sync.Mutex
	claimed bool
}

func New(ops configops.Service, restart func()) Service {
	return &svc{ops: ops, restart: restart}
}

func (a *svc) Ops() configops.Service { return a.ops }

// PendingCall reads the marker that survives the restart caused by its apply.
func (a *svc) PendingCall(sessionID int64) (configops.Pending, error) {
	p, err := a.ops.LoadPending()
	if err != nil {
		return configops.Pending{}, fmt.Errorf("load pending-apply marker: %w", err)
	}

	if p == nil || p.SessionID != sessionID {
		return configops.Pending{}, nil
	}

	return *p, nil
}

// ClaimApply reserves the process-wide marker and restart slot.
func (a *svc) ClaimApply() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.claimed {
		return false
	}

	a.claimed = true

	return true
}

// ReleaseApply returns a claim only when no commit reached disk.
func (a *svc) ReleaseApply() {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.claimed = false
}

// Apply keeps a successful claim until the requested restart replaces the process.
func (a *svc) Apply(staged *configops.Staged, p configops.Pending) configops.Verdict {
	if v := a.ops.Commit(staged, p); v.Failed() {
		a.ReleaseApply()

		return v
	}

	a.Restart()

	return configops.OK()
}

// Restart also supports credential changes that leave config.yaml untouched.
func (a *svc) Restart() {
	if a.restart != nil {
		a.restart()
	}
}
