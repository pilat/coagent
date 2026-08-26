package sessionstore

import (
	"database/sql"
	"fmt"
	"maps"
)

func requireOneSessionUpdate(result sql.Result, sessionID int64) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("session update rows affected: %w", err)
	}

	if rows != 1 {
		return fmt.Errorf("session %d not found or already terminal", sessionID)
	}

	return nil
}

func cloneAttributes(attributes map[string]any) map[string]any {
	cloned := make(map[string]any, len(attributes))
	maps.Copy(cloned, attributes)

	return cloned
}
