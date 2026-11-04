package sessionstore

import (
	"context"
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

// SetSessionTitle renames only live work roots owned by the manager, preserving list order.
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
