package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// SetOrchestrator changes only the mode on a live owned root, preserving other attributes.
func (s *Store) SetOrchestrator(ctx context.Context, managerID string, id int64, on bool) (bool, error) {
	update := `json_remove(attributes, '$.orchestrator')`
	predicate := `json_type(attributes, '$.orchestrator') = 'true'`

	if on {
		update = `json_set(COALESCE(attributes,'{}'), '$.orchestrator', json('true'))`
		predicate = `json_type(attributes, '$.orchestrator') IS NOT 'true'`
	}

	result, err := s.db.ExecContext(ctx, `UPDATE sessions AS s SET attributes = `+update+
		` WHERE s.id = ? AND `+liveChatRootSQL+` AND `+predicate, id, managerID)
	if err != nil {
		return false, fmt.Errorf("set orchestrator mode: %w", err)
	}

	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count orchestrator update: %w", err)
	}

	if changed == 0 {
		_, err = s.OrchestratorMode(ctx, managerID, id)
	}

	return changed != 0, err
}

// OrchestratorMode reads the mode only for a live root owned by the manager.
func (s *Store) OrchestratorMode(ctx context.Context, managerID string, id int64) (bool, error) {
	var on bool

	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(json_type(attributes, '$.orchestrator') = 'true',0)
		FROM sessions s WHERE s.id = ? AND `+liveChatRootSQL, id, managerID).Scan(&on)
	if errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("#%d is not a live session", id)
	}

	if err != nil {
		return false, fmt.Errorf("read orchestrator mode: %w", err)
	}

	return on, nil
}
