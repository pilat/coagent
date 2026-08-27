package session

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

// Single source of truth for a cleared result's rendered content — from
// tool_name alone, so the load and event-apply paths render byte-identically.
const clearedPlaceholderFormat = "[cleared: %s result removed to save context — re-run the tool if current data is needed]"

// clearedPlaceholder renders a cleared tool result. Shared by the load path
// (reloadMessages) and the event-apply path (applyClear) so both render alike.
func clearedPlaceholder(toolName string) string {
	return fmt.Sprintf(clearedPlaceholderFormat, toolName)
}

// applyClear replaces eligible tool-result bodies with a placeholder (0 = no-op).
// Compaction's first phase and nothing else: the transcript is never edited
// retroactively between compactions. Persists first, then substitutes in memory.
func (s *svc) applyClear(ctx context.Context, keepRecentRounds int) int {
	// pendingExternalCallIDs reads getMessages() (takes ms.mu, non-reentrant).
	external := s.pendingExternalCallIDs()

	s.ms.mu.Lock()
	defer s.ms.mu.Unlock()

	messages := s.ms.messages
	boundary := findMaskBoundary(messages, keepRecentRounds)

	var (
		targets    []int
		clearedIDs []int64
	)

	for i := range boundary {
		msg := messages[i]
		if !clearEligible(msg, external) || msg.Content == clearedPlaceholder(msg.ToolName) {
			continue
		}

		targets = append(targets, i)

		if msg.DBID != 0 {
			clearedIDs = append(clearedIDs, msg.DBID)
		}
	}

	if len(targets) == 0 {
		return 0
	}

	// Persist before mutating memory so a failed write can't leave a placeholder
	// that reloadMessages reverts (DB stays source of truth). In-memory-only rows
	// (DBID == 0) have nothing to persist and are substituted directly.
	if s.ms.store != nil && len(clearedIDs) > 0 {
		if err := s.ms.store.MarkCleared(ctx, clearedIDs); err != nil {
			logger.Ctx(ctx).Named("session.clear").Warn("mark_cleared_failed", zap.Error(err))
			return 0
		}
	}

	for _, i := range targets {
		messages[i].Content = clearedPlaceholder(messages[i].ToolName)
		// Refs must die here too: reloadMessages drops them for ClearedAt rows,
		// so keeping them in memory would diverge pre/post restart.
		messages[i].Images = nil
	}

	return len(targets)
}

// clearEligible: a tool result is clearable unless it's a skill envelope, a batch
// result carrying rendered skills, or an external-pending (sleep/task) result.
func clearEligible(msg llmwire.Message, external map[string]bool) bool {
	if msg.Role != llmwire.RoleTool {
		return false
	}

	if msg.ToolName == tool.IDSkill {
		return false
	}

	if external[msg.ToolCallID] {
		return false
	}

	if msg.ToolName == tool.IDBatch && len(builtin.ExtractRenderedSkills(msg.Content)) > 0 {
		return false
	}

	return true
}
