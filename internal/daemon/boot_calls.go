package daemon

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

const interruptedCallNotice = "⚠️ This tool call was interrupted before completion (e.g., daemon restart) " +
	"and was not re-executed — the operation may have partially completed. " +
	"Check the current state before retrying."

func (s *svc) recoverOrphanedCalls(ctx context.Context) error {
	records, err := s.store.ListAllSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions for orphan recovery: %w", err)
	}

	for _, record := range records {
		if !orphanSweepCandidate(record) {
			continue
		}

		calls, err := s.orphanedCalls(ctx, record.ID)
		if err != nil {
			return err
		}

		for _, call := range calls {
			if _, err := s.store.Enqueue(ctx, sessionstore.Input{
				SessionID: record.ID, Source: sessionstore.InputSourceCallResult,
				Content:     orphanedCallNotice(call.Name),
				Attributes:  map[string]any{"call_id": call.ID, "tool_id": call.Name},
				DeliveryKey: "orphan:" + call.ID,
			}); err != nil {
				return fmt.Errorf("cancel orphaned call %s: %w", call.ID, err)
			}
		}
	}

	return nil
}

func (s *svc) recoverInterruptedTools(ctx context.Context) error {
	records, err := s.store.ListAllSessions(ctx)
	if err != nil {
		return fmt.Errorf("list sessions for interrupted tools: %w", err)
	}

	for _, record := range records {
		if !orphanSweepCandidate(record) {
			continue
		}

		messages, err := s.store.LoadActiveMessages(ctx, record.ID)
		if err != nil {
			return fmt.Errorf("load interrupted transcript %d: %w", record.ID, err)
		}

		unresolved, err := session.UnresolvedStoredCalls(messages)
		if err != nil {
			return err
		}

		calls := make([]session.PendingToolCall, 0, len(unresolved))
		for _, call := range unresolved {
			if !tool.IsExternalCall(call.Name) {
				calls = append(calls, call)
			}
		}

		if len(calls) == 0 {
			continue
		}

		if _, err := s.store.Commit(ctx, sessionstore.Commit{
			SessionID: record.ID, Mode: sessionstore.CommitLifecycle,
			ToolResults: session.SettleResults(calls, interruptedCallNotice),
		}); err != nil {
			return fmt.Errorf("settle interrupted tools %d: %w", record.ID, err)
		}
	}

	return nil
}
