package sessionstore

import (
	"context"
	"fmt"
	"time"
)

// ReactivateForSchedule wakes a stopped root only when a due occurrence survived stop.
func (s *Store) ReactivateForSchedule(ctx context.Context, sessionID int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET status = 'active', updated_at = ?
		WHERE id = ? AND parent_id = 0 AND status = 'stopped' AND killed_at IS NULL
		AND EXISTS (SELECT 1 FROM session_inbox WHERE session_id = sessions.id
			AND state = 'pending' AND source = 'schedule')`, time.Now().UTC(), sessionID)
	if err != nil {
		return false, fmt.Errorf("reactivate scheduled root %d: %w", sessionID, err)
	}

	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("reactivation rows affected: %w", err)
	}

	if changed > 0 {
		s.recordWoken(sessionID)
	}

	return changed > 0, nil
}
