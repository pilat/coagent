package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/controllerapi"
)

const TitleAttribute = controllerapi.SessionAttributeTitle

// NormalizeTitle validates the shared session title contract.
func NormalizeTitle(raw string) (string, bool) {
	return controllerapi.NormalizeSessionTitle(raw)
}

// TitleFromInput derives a title from accepted user input.
func TitleFromInput(content string) string {
	return controllerapi.SessionTitleFromInput(content)
}

// SetSessionTitle renames only live roots owned by the manager, preserving list order.
func (s *Store) SetSessionTitle(ctx context.Context, managerID string, sessionID int64, raw string) error {
	title, ok := NormalizeTitle(raw)
	if !ok {
		return errors.New("title must be one line of 1-80 characters")
	}

	result, err := s.db.ExecContext(ctx, `UPDATE sessions AS s
		SET attributes = json_set(COALESCE(attributes, '{}'), '$.title', ?)
		WHERE s.id = ? AND `+liveChatRootSQL, title, sessionID, managerID)
	if err != nil {
		return fmt.Errorf("set session title: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count session title update: %w", err)
	}

	if affected == 0 {
		return fmt.Errorf("#%d is not a live session", sessionID)
	}

	return nil
}

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
