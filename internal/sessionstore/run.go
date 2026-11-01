package sessionstore

import (
	"context"
	"database/sql"
	"fmt"
)

func beginRunTx(ctx context.Context, tx *sql.Tx, sessionID int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE sessions
		SET attributes = json_set(COALESCE(attributes, '{}'), '$.current_run',
			COALESCE(json_extract(attributes, '$.current_run'), 0) + 1)
		WHERE id = ?`, sessionID)
	if err != nil {
		return fmt.Errorf("begin session run: %w", err)
	}

	return nil
}
