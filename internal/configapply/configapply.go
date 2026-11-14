package configapply

import (
	"context"
	"errors"
	"sync"

	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/tool"
)

const appliedRestartNote = "The daemon restarts as soon as this reply ends; reply briefly and make no further tool calls. " +
	"If it cannot start on the new configuration, it rolls back and the user is told."

// Service serializes configuration changes and requests restart after the applying run ends.
type Service struct {
	ops         configops.Service
	restart     chan struct{}
	mu          sync.Mutex
	claimed     bool
	appliedBy   int64
	restartSent bool
}

func New(ops configops.Service) *Service {
	return &Service{ops: ops, restart: make(chan struct{}, 1)}
}

func (a *Service) Ops() configops.Service { return a.ops }

func (a *Service) Restart() <-chan struct{} { return a.restart }

// RestartIfApplied signals once, after the session that wrote the config finishes its run.
func (a *Service) RestartIfApplied(sessionID int64) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.appliedBy == 0 || a.appliedBy != sessionID || a.restartSent {
		return
	}

	a.restartSent = true
	select {
	case a.restart <- struct{}{}:
	default:
	}
}

func (a *Service) apply(ctx context.Context, sessionID int64, staged *configops.Staged) (*tool.Result, error) {
	a.mu.Lock()
	if a.claimed {
		a.mu.Unlock()
		return nil, errors.New("another config change is already being applied — make one change at a time")
	}

	a.claimed = true
	a.mu.Unlock()

	applied := false
	defer func() {
		if !applied {
			a.mu.Lock()
			a.claimed = false
			a.mu.Unlock()
		}
	}()

	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	if v := a.ops.Commit(staged, configops.Pending{SessionID: sessionID}); v.Failed() {
		return nil, errors.New("Config change rejected — " + v.Reason() + "\n" + RefusalReminder)
	}

	a.mu.Lock()
	a.appliedBy = sessionID
	a.mu.Unlock()

	applied = true

	return &tool.Result{
		Output:         "Config applied: " + staged.Summary + ". " + appliedRestartNote,
		DirectMessages: []string{"Config applied — the daemon restarts to load it."},
	}, nil
}
