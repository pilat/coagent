package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/tool"
)

// subagentWaitGuard makes the background-child contract executable. Prompt
// text alone cannot prevent a model from using sleep as a polling loop; while a
// child completion is already an automatic wake source, yielding the turn is
// the only valid wait operation.
type subagentWaitGuard struct {
	inner      tool.Tool
	hasPending func(context.Context) (bool, error)
}

func (g *subagentWaitGuard) ID() string                  { return g.inner.ID() }
func (g *subagentWaitGuard) Description() string         { return g.inner.Description() }
func (g *subagentWaitGuard) Parameters() json.RawMessage { return g.inner.Parameters() }

func (g *subagentWaitGuard) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	pending, err := g.hasPending(ctx)
	if err != nil {
		return nil, fmt.Errorf("check pending subagents before %s: %w", g.inner.ID(), err)
	}

	if pending {
		return nil, errors.New(
			"sleep is unavailable while a subagent completion is pending: end this response or continue independent work; the subagent will wake the session automatically",
		)
	}

	return g.inner.Execute(ctx, params)
}

func (s *svc) guardSleepWhileSubagentsPending(sessionID int64, inner tool.Tool) tool.Tool {
	return &subagentWaitGuard{
		inner: inner,
		hasPending: func(ctx context.Context) (bool, error) {
			links, err := s.links.ListPendingChildLinks(ctx, sessionID)
			if err != nil {
				return false, fmt.Errorf("list pending child links: %w", err)
			}

			return len(links) > 0, nil
		},
	}
}
