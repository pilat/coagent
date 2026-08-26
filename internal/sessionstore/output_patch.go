package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func validateMessageIDs(ids []string) error {
	if ids == nil {
		return nil
	}

	seen := make(map[string]struct{}, len(ids))
	for _, value := range ids {
		if value == "" {
			return errors.New("empty message id")
		}

		if _, ok := seen[value]; ok {
			return errors.New("duplicate message id")
		}

		seen[value] = struct{}{}
	}

	return nil
}

func patchOutputSessionAttributes(
	ctx context.Context,
	tx *sql.Tx,
	sessionID int64,
	managerID string,
	patch map[string]any,
) error {
	if len(patch) == 0 {
		return nil
	}

	attributes, err := outputSessionAttributes(ctx, tx, sessionID)
	if err != nil {
		return err
	}

	for key, value := range patch {
		if key == managerIDAttribute || key == "channel" || value == nil || key == "" {
			return fmt.Errorf("invalid session output patch %q", key)
		}

		attributes[key] = value
	}

	if attributes[managerIDAttribute] != managerID {
		return ErrOutputAttempt
	}

	encoded, err := json.Marshal(attributes)
	if err != nil {
		return fmt.Errorf("marshal output session patch: %w", err)
	}

	if _, err := tx.ExecContext(
		ctx,
		`UPDATE sessions SET attributes = ?, updated_at = ? WHERE id = ?`,
		string(encoded),
		time.Now().UTC(),
		sessionID,
	); err != nil {
		return fmt.Errorf("patch output session attributes: %w", err)
	}

	return nil
}
