package sessionstore

import (
	"context"
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
