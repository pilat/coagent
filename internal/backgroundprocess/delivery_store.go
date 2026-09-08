package backgroundprocess

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ClaimDelivery selects and claims the completion target atomically.
func (s *store) ClaimDelivery(ctx context.Context, id string) (int64, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("claim process delivery: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	var target int64

	err = tx.QueryRowContext(ctx, `
		UPDATE background_processes
		SET delivery_state = 'claimed',
			delivery_target_session_id = CASE
				WHEN session_id = root_session_id THEN root_session_id
				WHEN EXISTS (
					SELECT 1 FROM sessions
					WHERE sessions.id = background_processes.session_id
					  AND sessions.parent_id <> 0
					  AND sessions.status IN ('active', 'suspended')
				) THEN session_id
				ELSE root_session_id
			END
		WHERE id = ?
		  AND state <> 'running'
		  AND advertised_at IS NOT NULL
		  AND host_intent NOT IN ('session_stopped', 'session_killed')
		  AND delivery_state = 'pending'
		RETURNING delivery_target_session_id`, id,
	).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}

	if err != nil {
		return 0, false, fmt.Errorf("claim process delivery: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("claim process delivery: %w", err)
	}

	return target, true, nil
}

func (s *store) MarkDelivered(ctx context.Context, id string) (bool, error) {
	return s.completeDelivery(ctx, id, "delivered")
}

func (s *store) MarkSuppressed(ctx context.Context, id string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE background_processes
		SET delivery_state = 'suppressed',
			delivery_target_session_id = COALESCE(delivery_target_session_id, root_session_id),
			delivered_at = ?
		WHERE id = ? AND delivery_state IN ('pending', 'claimed')`, time.Now().UTC(), id,
	)
	if err != nil {
		return false, fmt.Errorf("suppress process delivery: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("suppress process delivery: %w", err)
	}

	return rows == 1, nil
}

// ListUndelivered returns advertised terminal completions still owed to a session.
func (s *store) ListUndelivered(ctx context.Context) ([]Process, error) {
	return s.list(ctx,
		`SELECT `+processColumns+` FROM background_processes
		 WHERE state <> 'running'
		   AND delivery_state IN ('pending', 'claimed')
		   AND advertised_at IS NOT NULL`,
	)
}

func (s *store) ListUndeliveredForTarget(
	ctx context.Context,
	targetSessionID int64,
) ([]Process, error) {
	return s.list(ctx,
		`SELECT `+processColumns+` FROM background_processes
		 WHERE state <> 'running'
		   AND delivery_state = 'claimed'
		   AND delivery_target_session_id = ?
		   AND advertised_at IS NOT NULL`, targetSessionID,
	)
}

func (s *store) CountTerminalByIntentSince(
	ctx context.Context,
	rootSessionID int64,
	intent HostIntent,
	since time.Time,
) (int, error) {
	var count int

	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM background_processes
		WHERE root_session_id = ? AND host_intent = ?
		  AND state <> 'running' AND finished_at >= ?`,
		rootSessionID, string(intent), since,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count terminal processes by intent: %w", err)
	}

	return count, nil
}

func (s *store) completeDelivery(ctx context.Context, id, deliveryState string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE background_processes
		SET delivery_state = ?, delivered_at = ?
		WHERE id = ? AND delivery_state = 'claimed'`,
		deliveryState, time.Now().UTC(), id,
	)
	if err != nil {
		return false, fmt.Errorf("complete process delivery: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("complete process delivery: %w", err)
	}

	return rows == 1, nil
}
