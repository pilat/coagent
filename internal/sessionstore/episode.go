package sessionstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

func activateAgentInputTx(ctx context.Context, tx *sql.Tx, id int64, at time.Time) error {
	var fired bool

	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_budgets
		WHERE root_session_id = ? AND state = 'fired')`, id).Scan(&fired)
	if err != nil {
		return fmt.Errorf("check agent input budget: %w", err)
	}

	if fired {
		return nil
	}

	return activateManagerInputTx(ctx, tx, id, at)
}
