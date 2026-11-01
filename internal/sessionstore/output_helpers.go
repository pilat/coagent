package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

func outputOwner(ctx context.Context, tx *sql.Tx, sessionID int64) (string, error) {
	var parentID int64
	var attributes string

	err := tx.QueryRowContext(ctx, `SELECT parent_id, attributes FROM sessions WHERE id = ?`, sessionID).
		Scan(&parentID, &attributes)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("session %d not found", sessionID)
	}

	if err != nil {
		return "", fmt.Errorf("load output owner: %w", err)
	}

	if parentID != 0 {
		return "", ErrOutputNotRoot
	}

	var values map[string]any
	if err := json.Unmarshal([]byte(attributes), &values); err != nil {
		return "", fmt.Errorf("decode session attributes: %w", err)
	}

	owner, _ := values[managerIDAttribute].(string)
	if owner == "" {
		return "", ErrOutputOwner
	}

	return owner, nil
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
