package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

func (s *svc) hasPendingDurableInput(ctx context.Context, sessionID int64) (bool, error) {
	_, err := s.inboxStore.PeekPending(ctx, sessionID)
	if errors.Is(err, sessionstore.ErrNoPendingInput) {
		return false, nil
	}

	if err != nil {
		return false, fmt.Errorf("peek durable input for session %d: %w", sessionID, err)
	}

	return true, nil
}

// pendingInputRunnable distinguishes input that can advance now from input
// durably queued behind an external call. A normal message may interrupt sleep;
// it cannot jump a foreground subagent/config/secret result.
func (s *svc) pendingInputRunnable(ctx context.Context, sessionID int64) (bool, error) {
	pending, err := s.hasPendingDurableInput(ctx, sessionID)
	if err != nil || !pending {
		return pending, err
	}

	calls, err := s.pendingExternalCallsForSession(ctx, sessionID)
	if err != nil {
		return false, err
	}

	for _, name := range calls {
		if name != tool.IDSleep {
			return false, nil
		}
	}

	return true, nil
}

// recoverableInputRunnable accepts both forms PASS 3 owns: an inbox row still
// pending, or an active turn backed by a durable accepted-message identity.
func (s *svc) recoverableInputRunnable(ctx context.Context, sessionID int64) (bool, error) {
	record, err := s.sessionStore.GetSession(ctx, sessionID)
	if err != nil {
		return false, fmt.Errorf("load recoverable session %d: %w", sessionID, err)
	}

	if record.Status == sessionstore.SessionStatusStopped || record.Status == sessionstore.SessionStatusError {
		sessionIDs, listErr := s.inboxStore.ListSessionsWithRecoverableInput(ctx)
		if listErr != nil {
			return false, fmt.Errorf("classify recoverable session %d: %w", sessionID, listErr)
		}

		if !slices.Contains(sessionIDs, sessionID) {
			return false, nil
		}
	}

	pending, err := s.hasPendingDurableInput(ctx, sessionID)
	if err != nil {
		return false, err
	}

	if pending {
		return s.pendingInputRunnable(ctx, sessionID)
	}

	accepted, err := s.hasAcceptedInput(ctx, record)
	if err != nil {
		return false, err
	}

	if !accepted {
		return false, nil
	}

	calls, err := s.pendingExternalCallsForSession(ctx, sessionID)
	if err != nil {
		return false, err
	}

	return len(calls) == 0, nil
}

func (s *svc) hasAcceptedInput(
	ctx context.Context,
	rec *sessionstore.SessionRecord,
) (bool, error) {
	if rec.KilledAt != nil || rec.Status != sessionstore.SessionStatusActive {
		return false, nil
	}

	accepted, err := s.inboxStore.HasAcceptedInput(ctx, rec.ID)
	if err != nil {
		return false, fmt.Errorf("load accepted input identity for session %d: %w", rec.ID, err)
	}

	return accepted, nil
}
