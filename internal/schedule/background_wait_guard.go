package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

var _ tool.Tool = (*backgroundWaitGuard)(nil)

type backgroundWaitGuard struct {
	inner     tool.Tool
	sessions  *sessionstore.Store
	sessionID int64
}

// NewGuardedSleepTool prevents polling while a durable producer owns the wake-up.
func NewGuardedSleepTool(svc Service, sessionID int64, sessions *sessionstore.Store) tool.Tool {
	return &backgroundWaitGuard{inner: NewSleepTool(svc, sessionID), sessions: sessions, sessionID: sessionID}
}

func (g *backgroundWaitGuard) ID() string { return g.inner.ID() }

func (g *backgroundWaitGuard) Description() string { return g.inner.Description() }

func (g *backgroundWaitGuard) Parameters() json.RawMessage { return g.inner.Parameters() }

func (g *backgroundWaitGuard) ParallelSafe() bool { return g.inner.ParallelSafe() }

func (g *backgroundWaitGuard) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	pending, err := g.sessions.HasPendingBackgroundWait(ctx, g.sessionID)
	if err != nil {
		return nil, fmt.Errorf("check pending background work before %s: %w", g.inner.ID(), err)
	}
	if pending {
		return nil, errors.New(
			"sleep is unavailable while background completion is pending; do not poll. " +
				"Do not poll with sleep or another timer. Continue only useful independent work. " +
				"When none remains, briefly report what is still running and end the response; the result arrives automatically in a later turn",
		)
	}
	return g.inner.Execute(ctx, params)
}
