package daemon

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/tool"
)

// Pending uses producer identities; a tool name or dangling transcript call cannot prove ownership.
func (s *externalCalls) Pending(ctx context.Context, sessionID int64) (map[string]string, error) {
	calls := s.staged.forSession(sessionID)
	if calls == nil {
		calls = make(map[string]string)
	}

	if s.applier != nil {
		owed, err := s.applier.PendingCall(sessionID)

		switch {
		case err != nil:
			logger.Ctx(ctx).Named("daemon.runner").
				Warn("read_pending_apply_marker", zap.Int64("session_id", sessionID), zap.Error(err))
		case owed.ToolCallID != "":
			calls[owed.ToolCallID] = owed.ToolName
		}
	}

	if s.scheduleSvc != nil {
		sleeps, err := s.scheduleSvc.PendingSleeps(ctx, sessionID)
		if err != nil {
			return nil, fmt.Errorf("load pending sleeps for session %d: %w", sessionID, err)
		}

		for _, sleep := range sleeps {
			calls[sleep.CallID] = tool.IDSleep
		}
	}

	links, err := s.links.ListPendingChildLinks(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load pending child calls for session %d: %w", sessionID, err)
	}

	for _, link := range links {
		if link.Blocking && link.TaskCallID != "" {
			calls[link.TaskCallID] = tool.IDTask
		}
	}

	return calls, nil
}
