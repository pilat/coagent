package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ActiveUserInput returns the newest accepted user input in the active transcript.
func (s *Store) ActiveUserInput(ctx context.Context, sessionID int64) (string, error) {
	var text string

	err := s.db.QueryRowContext(ctx, `SELECT i.raw_content FROM session_inbox i
JOIN messages m ON m.id = i.accepted_message_id AND m.session_id = i.session_id
WHERE i.session_id = ? AND i.source = 'user' AND i.state = 'accepted'
AND m.compacted_at IS NULL ORDER BY i.id DESC LIMIT 1`, sessionID).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("load active user input: %w", err)
	}

	return text, nil
}
