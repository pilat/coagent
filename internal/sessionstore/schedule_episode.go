package sessionstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

func startRootEpisode(ctx context.Context, tx *sql.Tx, id int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE sessions SET episode_started_at = ? WHERE id = ? AND parent_id = 0
  AND (episode_started_at IS NULL OR status NOT IN ('active','suspended'))`, time.Now().UTC(), id)
	if err != nil {
		return fmt.Errorf("start manager episode: %w", err)
	}

	return nil
}
