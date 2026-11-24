package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

func requireOutputRoot(ctx context.Context, tx *sql.Tx, sessionID int64) error {
	var parentID int64

	err := tx.QueryRowContext(ctx, `SELECT parent_id FROM sessions WHERE id = ?`, sessionID).
		Scan(&parentID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("session %d not found", sessionID)
	}

	if err != nil {
		return fmt.Errorf("load output root: %w", err)
	}

	if parentID != 0 {
		return ErrOutputNotRoot
	}

	return nil
}

func outputSessionWritable(ctx context.Context, tx *sql.Tx, sessionID int64) error {
	var status SessionStatus

	var killedAt sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT status, killed_at FROM sessions WHERE id = ?`, sessionID).
		Scan(&status, &killedAt); err != nil {
		return fmt.Errorf("load output session state: %w", err)
	}

	if killedAt.Valid || status == SessionStatusStopping || status == SessionStatusTerminating ||
		status == SessionStatusKilled {
		return fmt.Errorf("session %d %w", sessionID, ErrOutputNotWritable)
	}

	return nil
}
