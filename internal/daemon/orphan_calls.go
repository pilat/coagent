package daemon

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
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

// orphanedCalls lists a session's unresolved external calls that no producer
// ledger claims, in transcript order.
func (s *svc) orphanedCalls(ctx context.Context, sessionID int64) ([]session.PendingToolCall, error) {
	stored, err := s.store.LoadActiveMessages(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load transcript of session %d: %w", sessionID, err)
	}

	pending, err := session.UnresolvedStoredCalls(stored)
	if err != nil {
		return nil, fmt.Errorf("scan transcript of session %d: %w", sessionID, err)
	}

	if len(pending) == 0 {
		return nil, nil
	}

	owners, err := s.callOwners(ctx, sessionID)
	if err != nil {
		return nil, err
	}

	orphans := make([]session.PendingToolCall, 0, len(pending))

	for _, call := range pending {
		if tool.IsExternalCall(call.Name) && owners[call.ID] != call.Name {
			orphans = append(orphans, call)
		}
	}

	return orphans, nil
}
