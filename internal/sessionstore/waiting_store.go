package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var ErrNoWaitingOutput = errors.New("session has no waiting output")

func (s *Store) LatestWaitingOutput(ctx context.Context, sessionID int64) (*OutputRecord, error) {
	record, err := scanOutputRecord(s.db.QueryRowContext(ctx, `SELECT `+outputColumns+` FROM session_outbox
		WHERE session_id = ? AND source_key LIKE 'wait:%'
		ORDER BY id DESC LIMIT 1`, sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoWaitingOutput
	}

	if err != nil {
		return nil, fmt.Errorf("load latest waiting output: %w", err)
	}

	return record, nil
}
