package backgroundprocess

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

func (s *store) finalizeRunning(
	ctx context.Context,
	tx *sql.Tx,
	id string,
	outcome State,
	exitCode *int,
	outputSize int64,
	fallbackIntent HostIntent,
) (Process, bool, error) {
	recordedExit := exitCode
	if outcome != StateCompleted && outcome != StateFailed {
		recordedExit = nil
	}

	now := time.Now().UTC()
	deliveryState := "pending"
	var deliveredAt any

	if outcome == StateCancelled {
		deliveryState = "suppressed"
		deliveredAt = now
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE background_processes
		SET state = ?, exit_code = ?, output_size = ?, finished_at = ?,
			host_intent = CASE WHEN host_intent = '' AND ? <> '' THEN ? ELSE host_intent END,
			delivery_state = CASE WHEN ? = 'suppressed' THEN 'suppressed' ELSE delivery_state END,
			delivery_target_session_id = CASE
				WHEN ? = 'suppressed' THEN root_session_id ELSE delivery_target_session_id END,
			delivered_at = CASE WHEN ? = 'suppressed' THEN ? ELSE delivered_at END
		WHERE id = ? AND state = 'running'`,
		string(outcome), recordedExit, outputSize, now, string(fallbackIntent), string(fallbackIntent),
		deliveryState, deliveryState, deliveryState, deliveredAt, id,
	)
	if err != nil {
		return Process{}, false, fmt.Errorf("finalize process: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return Process{}, false, fmt.Errorf("finalize process: %w", err)
	}

	if rows != 1 {
		return Process{}, false, nil
	}

	winner, err := scanProcess(tx.QueryRowContext(ctx,
		`SELECT `+processColumns+` FROM background_processes WHERE id = ?`, id,
	))
	if err != nil {
		return Process{}, false, fmt.Errorf("finalize process: %w", err)
	}

	return winner, true, nil
}
