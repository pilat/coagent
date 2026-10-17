package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// AcceptedUserMessageIDs marks transcript rows that came from a person or an assistant send,
// not from process completions or host notes.
func (s *Store) AcceptedUserMessageIDs(ctx context.Context, sessionID int64) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT accepted_message_id FROM session_inbox
		WHERE session_id = ? AND source = 'user' AND accepted_message_id IS NOT NULL ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("read accepted input message ids: %w", err)
	}
	defer rows.Close()

	var ids []int64

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan accepted input message id: %w", err)
		}

		ids = append(ids, id)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read accepted assistant inputs: %w", err)
	}

	return ids, nil
}

// LastUserInput is the latest accepted request from a person or an assistant send.
func (s *Store) LastUserInput(ctx context.Context, sessionID int64) (string, error) {
	var text string

	err := s.db.QueryRowContext(ctx, `SELECT raw_content FROM session_inbox
		WHERE session_id = ? AND source = 'user' AND state = 'accepted' ORDER BY id DESC LIMIT 1`, sessionID).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("read last user input: %w", err)
	}

	return text, nil
}

// LatestModelOutput excludes tool narration, lifecycle receipts and progress cards.
func (s *Store) LatestModelOutput(ctx context.Context, sessionID int64) (*OutputRecord, error) {
	row, err := scanOutputRecord(s.db.QueryRowContext(ctx, `SELECT `+outputColumns+` FROM session_outbox
		WHERE session_id = ? AND type = 'message_persistent' AND source_key LIKE 'message:%' ORDER BY id DESC LIMIT 1`, sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoOutput
	}

	return row, err
}

// SessionTreeCost excludes corrupt cost rows without discarding valid sibling usage.
func (s *Store) SessionTreeCost(ctx context.Context, root int64) (float64, error) {
	var cost float64

	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(m.cost_usd), 0)
		FROM messages m JOIN sessions s ON s.id = m.session_id
		WHERE (s.id = ? OR s.root_id = ?) AND m.cost_usd BETWEEN 0 AND 100`, root, root).Scan(&cost)
	if err != nil {
		return 0, fmt.Errorf("read session tree cost: %w", err)
	}

	return cost, nil
}
