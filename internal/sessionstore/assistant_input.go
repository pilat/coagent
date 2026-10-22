package sessionstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// HandleAssistantInputTx keeps assistant turns out of the ordinary transcript acceptance path; a turn resolves its input once.
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
		WHERE id = ? AND session_id = ? AND state = 'pending'`,
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
