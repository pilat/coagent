package assistant

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// RecordSendTx commits the observation obligation with the work input.
func RecordSendTx(ctx context.Context, tx *sql.Tx, send Send) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO assistant_sends
		(input_id, assistant_session_id, session_id, created_at) VALUES (?, ?, ?, ?)`,
		send.InputID, send.AssistantSessionID, send.SessionID, send.CreatedAt)
	if err != nil {
		return fmt.Errorf("record assistant send: %w", err)
	}

	return nil
}

// UnobservedSends returns FIFO obligations; sessionID zero selects the boot sweep.
func (s *store) UnobservedSends(ctx context.Context, sessionID int64) ([]Send, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT input_id, assistant_session_id, session_id, created_at
		FROM assistant_sends WHERE observed_at IS NULL AND (? = 0 OR session_id = ?) ORDER BY input_id`,
		sessionID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load assistant sends: %w", err)
	}
	defer rows.Close()

	var sends []Send

	for rows.Next() {
		var send Send
		if err := rows.Scan(&send.InputID, &send.AssistantSessionID, &send.SessionID, &send.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan assistant send: %w", err)
		}

		sends = append(sends, send)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read assistant sends: %w", err)
	}

	return sends, nil
}

// MarkObservedTx reports whether this transaction won settlement; only the winner may emit an event.
func MarkObservedTx(ctx context.Context, tx *sql.Tx, inputID int64, at time.Time) (bool, error) {
	result, err := tx.ExecContext(ctx, `UPDATE assistant_sends SET observed_at = ?
		WHERE input_id = ? AND observed_at IS NULL`, at, inputID)
	if err != nil {
		return false, fmt.Errorf("observe assistant send: %w", err)
	}

	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("observed assistant send rows: %w", err)
	}

	return changed > 0, nil
}

// RecordTurnTx keeps rejected responses as well as committed steps for inspection.
func RecordTurnTx(ctx context.Context, tx *sql.Tx, turn Turn) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO assistant_turns
		(session_id, input_id, step, desk, response, outcome, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		turn.SessionID, turn.InputID, turn.Step, turn.Desk, turn.Response, turn.Outcome, turn.CreatedAt)
	if err != nil {
		return fmt.Errorf("record assistant turn: %w", err)
	}

	return nil
}
