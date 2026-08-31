package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// LifecycleOutputHistoryStore supplies the lifecycle anchor for a repaired
// manager surface without exposing delivery receipts to producers.
type LifecycleOutputHistoryStore interface {
	LatestLifecycleOutputID(ctx context.Context, sessionID int64) (int64, error)
}

func (s *store) LatestLifecycleOutputID(ctx context.Context, sessionID int64) (int64, error) {
	var id sql.NullInt64

	err := s.db.QueryRowContext(ctx, `SELECT MAX(id) FROM session_outbox
		WHERE session_id = ? AND type IN ('session_opened', 'session_replaced', 'session_closed')`, sessionID).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("load latest lifecycle output: %w", err)
	}

	if !id.Valid {
		return 0, errors.New("session has no lifecycle output")
	}

	return id.Int64, nil
}
