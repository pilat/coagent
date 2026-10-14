package daemon

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

func (s *svc) refuseAssistantCommand(ctx context.Context, input *sessionstore.InboxInput) (bool, error) {
	record, err := s.store.GetSession(ctx, input.SessionID)
	if err != nil {
		return false, fmt.Errorf("load assistant command target: %w", err)
	}

	if record.AgentType != tool.AssistantAgentType {
		return false, nil
	}

	const text = "This command is unavailable in the Assistant topic."

	var changed bool

	err = s.store.WithTx(ctx, func(tx *sql.Tx) error {
		var rejectErr error

		changed, rejectErr = sessionstore.RejectAssistantCommandTx(ctx, tx, record.ID, input.ID, text)
		if rejectErr != nil {
			return fmt.Errorf("commit assistant refusal: %w", rejectErr)
		}

		return nil
	})
	if err != nil {
		return true, fmt.Errorf("refuse assistant command: %w", err)
	}

	if changed {
		s.publish(record.ID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: text})
	}

	return true, nil
}
