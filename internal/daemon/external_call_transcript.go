package daemon

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessioncalls"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

// storedExternalCalls is a session's name-keyed pending set, read from the
// durable transcript alone — what a provider would see dangling.
func (s *externalCalls) storedExternalCalls(ctx context.Context, sessionID int64) ([]session.PendingToolCall, error) {
	stored, err := s.sessionStore.LoadActiveMessages(ctx, sessionID)
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
func (s *externalCalls) orphanedCalls(ctx context.Context, sessionID int64) ([]session.PendingToolCall, error) {
	pending, err := s.storedExternalCalls(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	if len(pending) == 0 {
		return nil, nil
	}

	owners, err := s.Pending(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	orphans := make([]session.PendingToolCall, 0, len(pending))

	for _, call := range pending {
		if owners[call.ID] == "" {
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
	snapshot, err := sessioncalls.ScanStored(msgs)
	if err != nil {
		return nil, fmt.Errorf("scan stored calls: %w", err)
	}

	out := make([]session.PendingToolCall, 0, len(snapshot.GlobalUnresolved))
	for _, call := range snapshot.GlobalUnresolved {
		if matches(call.Name) {
			out = append(out, session.PendingToolCall{ID: call.ID, Name: call.Name})
		}
	}

	return out, nil
}
