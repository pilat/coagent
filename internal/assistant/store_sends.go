package assistant

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// send links an assistant instruction to the work session's durable input.
type send struct {
	inputID, root, sessionID int64
	createdAt                time.Time
}

// AwaitedSessions lists work sessions with unobserved sends, oldest obligation first.
func (s *Store) AwaitedSessions(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session_id FROM assistant_sends WHERE observed_at IS NULL
		GROUP BY session_id ORDER BY MIN(input_id)`)
	if err != nil {
		return nil, fmt.Errorf("load assistant recovery obligations: %w", err)
	}
	defer rows.Close()

	var ids []int64

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("load assistant recovery obligations: %w", err)
		}

		ids = append(ids, id)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load assistant recovery obligations: %w", err)
	}

	return ids, nil
}

// recordSendTx commits the observation obligation with the work input.
func recordSendTx(ctx context.Context, tx *sql.Tx, sd send) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO assistant_sends
		(input_id, assistant_session_id, session_id, created_at) VALUES (?, ?, ?, ?)`,
		sd.inputID, sd.root, sd.sessionID, sd.createdAt)
	if err != nil {
		return fmt.Errorf("record assistant send: %w", err)
	}

	return nil
}

// unobservedSends returns one session's obligations in FIFO order.
func (s *Store) unobservedSends(ctx context.Context, sessionID int64) ([]send, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT input_id, assistant_session_id, session_id, created_at
		FROM assistant_sends WHERE observed_at IS NULL AND session_id = ? ORDER BY input_id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("load assistant sends: %w", err)
	}
	defer rows.Close()

	var sends []send

	for rows.Next() {
		var sd send
		if err := rows.Scan(&sd.inputID, &sd.root, &sd.sessionID, &sd.createdAt); err != nil {
			return nil, fmt.Errorf("scan assistant send: %w", err)
		}

		sends = append(sends, sd)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read assistant sends: %w", err)
	}

	return sends, nil
}

// settleSends marks a stopped session's sends observed without emitting anything.
func (s *Store) settleSends(ctx context.Context, sessionID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE assistant_sends SET observed_at = ?
		WHERE session_id = ? AND observed_at IS NULL`, time.Now().UTC(), sessionID)
	if err != nil {
		return fmt.Errorf("settle stopped assistant sends: %w", err)
	}

	return nil
}

// markObservedTx reports whether this transaction won settlement; only the winner may emit an event.
func markObservedTx(ctx context.Context, tx *sql.Tx, inputID int64, at time.Time) (bool, error) {
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

// saveAboutTx commits why a work session exists together with its first instruction.
func saveAboutTx(ctx context.Context, tx *sql.Tx, sessionID int64, about string, at time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO assistant_work (session_id, about, created_at) VALUES (?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET about = excluded.about`, sessionID, about, at)
	if err != nil {
		return fmt.Errorf("save assistant work about: %w", err)
	}

	return nil
}
