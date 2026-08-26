package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// WaitingOutputStore exposes the last waiting projection for idempotent
// reconciliation. It is separate from delivery operations because producers
// only need the prior durable identity, never a manager receipt.
type WaitingOutputStore interface {
	LatestWaitingOutput(ctx context.Context, sessionID int64) (*OutputRecord, error)
}

var ErrNoWaitingOutput = errors.New("session has no waiting output")

func (s *store) LatestWaitingOutput(ctx context.Context, sessionID int64) (*OutputRecord, error) {
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
