package daemon

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

// orphanSweepCandidate skips lifecycles this pass does not own: /stop settles a
// parked tree from the same durable set, and killed or finished is not resumed.
func orphanSweepCandidate(rec *sessionstore.SessionRecord) bool {
	if rec.KilledAt != nil {
		return false
	}

	return rec.Status == sessionstore.SessionStatusActive ||
		rec.Status == sessionstore.SessionStatusSuspended ||
		rec.Status == sessionstore.SessionStatusError
}

// orphanedCallNotice is the deliberate cancellation an unowned call is answered
// with — an owned outcome, not a repair stub.
func orphanedCallNotice(_ string) string {
	return "The daemon restarted while this call was out with the world, and its producer did not survive. " +
		"The outcome is unknown — check the current state before retrying."
}

// storedExternalCalls is a session's name-keyed pending set, read from the
// durable transcript alone — what a provider would see dangling.
func (s *svc) storedExternalCalls(ctx context.Context, sessionID int64) ([]session.PendingToolCall, error) {
	stored, err := s.store.LoadActiveMessages(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load transcript of session %d: %w", sessionID, err)
	}

	pending, err := unresolvedStoredExternalCalls(stored)
	if err != nil {
		return nil, fmt.Errorf("scan transcript of session %d: %w", sessionID, err)
	}

	return pending, nil
}

// orphanedCalls lists a session's unresolved external calls that no producer
// ledger claims, in transcript order.
func (s *svc) orphanedCalls(ctx context.Context, sessionID int64) ([]session.PendingToolCall, error) {
	pending, err := s.storedExternalCalls(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	if len(pending) == 0 {
		return nil, nil
	}

	owners, err := s.pendingExternalCallsForSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	orphans := make([]session.PendingToolCall, 0, len(pending))

	for _, call := range pending {
		if owners[call.ID] != call.Name {
			orphans = append(orphans, call)
		}
	}

	return orphans, nil
}

// unresolvedStoredExternalCalls is the name-keyed pending set read straight from
// the durable transcript — what a provider would see dangling.
func unresolvedStoredExternalCalls(msgs []*transcript.Message) ([]session.PendingToolCall, error) {
	return unresolvedStoredCalls(msgs, tool.IsExternalCall)
}

// unresolvedStoredCalls lists unresolved calls matching the predicate, in
// transcript order.
func unresolvedStoredCalls(
	msgs []*transcript.Message,
	matches func(name string) bool,
) ([]session.PendingToolCall, error) {
	seen := make(map[string]bool)

	for _, m := range msgs {
		if m.Role == llmwire.RoleTool && m.ToolCallID != "" {
			seen[m.ToolCallID] = true
		}
	}

	var out []session.PendingToolCall

	for _, m := range msgs {
		if m.Role != llmwire.RoleAssistant || len(m.ToolCalls) == 0 {
			continue
		}

		var calls []llmwire.ToolCall
		if err := json.Unmarshal(m.ToolCalls, &calls); err != nil {
			return nil, fmt.Errorf("decode tool calls of message %d: %w", m.ID, err)
		}

		for _, tc := range calls {
			if tc.ID == "" || seen[tc.ID] || !matches(tc.Name) {
				continue
			}

			seen[tc.ID] = true
			out = append(out, session.PendingToolCall{ID: tc.ID, Name: tc.Name})
		}
	}

	return out, nil
}
