package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ActiveHumanInput returns the newest human input accepted in the current run.
func (s *Store) ActiveHumanInput(ctx context.Context, sessionID int64) (*InboxInput, error) {
	return activeHumanInput(ctx, s.db, sessionID)
}

func activeHumanInput(ctx context.Context, q queryer, sessionID int64) (*InboxInput, error) {
	input, err := scanInboxInput(q.QueryRowContext(ctx, `SELECT `+inboxColumns+` FROM session_inbox
WHERE session_id = ? AND source = 'user' AND state = 'accepted'
AND COALESCE(json_extract(attributes, '$.from'), '') <> 'assistant'
AND json_extract(attributes, '$.accepted_run') = (
 SELECT json_extract(attributes, '$.current_run') FROM sessions WHERE id = ?)
ORDER BY id DESC LIMIT 1`, sessionID, sessionID))
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load active human input: %w", err)
	}

	return input, nil
}
