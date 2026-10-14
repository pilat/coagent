package sessionstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

var _ AssistantInbox = (*Store)(nil)

// AssistantInbox shares lifecycle and inbox transactions with the independent desk loop.
type AssistantInbox interface {
	WithTx(context.Context, func(*sql.Tx) error) error
	ListPending(context.Context, int64) ([]*InboxInput, error)
	GetSession(context.Context, int64) (*SessionRecord, error)
	UpdateSessionStatus(context.Context, int64, SessionStatus) error
}

// RejectAssistantCommandTx lets the lifecycle refusal win over a concurrent ignored-command receipt.
func RejectAssistantCommandTx(ctx context.Context, tx *sql.Tx, sessionID, inputID int64, text string) (bool, error) {
	result, err := tx.ExecContext(ctx, `UPDATE session_inbox
		SET state = 'rejected', resolved_at = ?, resolution_reason = 'assistant command refused'
		WHERE id = ? AND session_id = ? AND (state = 'pending' OR
		(state = 'handled' AND resolution_reason = 'assistant_ignored_command'))`, time.Now().UTC(), inputID, sessionID)
	if err != nil {
		return false, fmt.Errorf("reject assistant command: %w", err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rejected assistant command count: %w", err)
	}

	if count == 0 {
		return false, nil
	}

	if err := PublishAssistantMessageTx(ctx, tx, sessionID, text); err != nil {
		return false, err
	}

	return true, nil
}

// HandleAssistantInputTx keeps desk steps out of the ordinary transcript acceptance path.
func HandleAssistantInputTx(
	ctx context.Context,
	tx *sql.Tx,
	sessionID, inputID int64,
	reason string,
	at time.Time,
) error {
	result, err := tx.ExecContext(
		ctx,
		`UPDATE session_inbox SET state = 'handled', resolved_at = ?, resolution_reason = ?
		WHERE id = ? AND session_id = ? AND (state = 'pending' OR
		(state = 'handled' AND resolution_reason = 'assistant_turn'))`,
		at,
		reason,
		inputID,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("handle assistant input: %w", err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("handled assistant input count: %w", err)
	}

	if count != 1 {
		return ErrInputResolved
	}

	return nil
}
