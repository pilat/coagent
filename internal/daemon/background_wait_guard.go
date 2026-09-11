package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/tool"
)

// backgroundWaitGuard rejects timer polling while a durable producer already owns the wake-up.
type backgroundWaitGuard struct {
	inner      tool.Tool
	hasPending func(context.Context) (bool, error)
}

func (g *backgroundWaitGuard) ID() string                  { return g.inner.ID() }
func (g *backgroundWaitGuard) Description() string         { return g.inner.Description() }
func (g *backgroundWaitGuard) Parameters() json.RawMessage { return g.inner.Parameters() }
func (g *backgroundWaitGuard) ParallelSafe() bool          { return g.inner.ParallelSafe() }

func (g *backgroundWaitGuard) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	pending, err := g.hasPending(ctx)
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

func (s *svc) guardSleepWhileBackgroundPending(sessionID int64, inner tool.Tool) tool.Tool {
	return &backgroundWaitGuard{
		inner: inner,
		hasPending: func(ctx context.Context) (bool, error) {
			links, err := s.links.ListPendingChildLinks(ctx, sessionID)
			if err != nil {
				return false, fmt.Errorf("list pending child links: %w", err)
			}

			if len(links) > 0 {
				return true, nil
			}

			if s.processStore == nil {
				return false, nil
			}

			processes, err := s.processStore.ListRunningBySessions(ctx, []int64{sessionID})
			if err != nil {
				return false, fmt.Errorf("list running processes: %w", err)
			}

			for _, process := range processes {
				if process.AdvertisedAt != nil {
					return true, nil
				}
			}

			return false, nil
		},
	}
}
