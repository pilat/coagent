package daemon

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

// settleParkedCompactions runs after the stop admission fence and runner joins,
// before generic stop cancellation can discard an accepted command.
func (s *svc) settleParkedCompactions(ctx context.Context, sessionIDs []int64) error {
	for _, sessionID := range sessionIDs {
		record, err := s.sessionStore.GetSession(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("load session %d for parked compaction: %w", sessionID, err)
		}

		inputs, err := s.inboxStore.ListPendingUserInputs(ctx, sessionID)
		if err != nil {
			return fmt.Errorf("list session %d parked commands: %w", sessionID, err)
		}

		for _, input := range inputs {
			if !isCompactCommand(input.RawContent) {
				continue
			}

			if err := s.settleParkedCompaction(ctx, record, input); err != nil {
				return fmt.Errorf("settle parked compact input %d: %w", input.ID, err)
			}
		}
	}

	return nil
}

func (s *svc) settleParkedCompaction(
	ctx context.Context,
	record *sessionstore.SessionRecord,
	input *sessionstore.InboxInput,
) error {
	if record.ParentID == 0 && !ownerlessSession(record) {
		_, err := s.lifecycleStore.HandleInputWithOutput(ctx, input.ID, "compact command",
			session.ParkedCompactionOutputDraft(record.ID, input.ID))
		if err != nil {
			return fmt.Errorf("handle parked compact input with output: %w", err)
		}

		return nil
	}

	if err := s.inboxStore.HandleInput(ctx, input.ID, "compact command"); err != nil {
		return fmt.Errorf("handle parked compact input: %w", err)
	}

	if record.ParentID == 0 {
		s.routes.Publish(record.ID, sessionevent.Notification{
			Type: sessionevent.NotifyMessage, Message: session.ParkedCompactionNotice,
		})
	}

	return nil
}
