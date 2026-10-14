package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// EnsureAssistantRoot preserves one durable desk identity per manager.
func (s *Store) EnsureAssistantRoot(
	ctx context.Context,
	projectID int64,
	owner, workDir string,
) (*SessionRecord, error) {
	if projectID <= 0 || owner == "" || workDir == "" {
		return nil, errors.New("assistant root requires project, owner and work dir")
	}

	var record *SessionRecord

	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		var err error

		record, err = scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions
			WHERE agent_type = 'assistant' AND parent_id = 0 AND json_extract(attributes, '$.manager_id') = ?`, owner))
		if !errors.Is(err, errSessionNotFound) {
			return err
		}

		attrs, err := json.Marshal(map[string]any{managerIDAttribute: owner})
		if err != nil {
			return fmt.Errorf("encode assistant owner: %w", err)
		}

		now := time.Now().UTC()

		result, err := tx.ExecContext(ctx, `INSERT INTO sessions
			(project_id, model, reasoning_level, attributes, agent_type, status, created_at, updated_at)
			VALUES (?, '', '', ?, 'assistant', 'completed', ?, ?)`, projectID, string(attrs), now, now)
		if err != nil {
			return fmt.Errorf("insert assistant root: %w", err)
		}

		id, err := result.LastInsertId()
		if err != nil {
			return fmt.Errorf("assistant root id: %w", err)
		}

		if _, err := insertLifecycleOutput(ctx, tx, id, OutputSessionOpened, "",
			map[string]any{outputAttributeName: "Assistant", outputAttributeWorkDir: workDir},
			fmt.Sprintf("session:%d:opened", id), now); err != nil {
			return err
		}

		record, err = scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id))

		return err
	})

	return record, err
}

// PublishAssistantMessageTx makes the reply durable in the desk step's transaction.
func PublishAssistantMessageTx(ctx context.Context, tx *sql.Tx, sessionID int64, text string) error {
	if err := outputSessionWritable(ctx, tx, sessionID); err != nil {
		return err
	}

	_, err := insertLifecycleOutput(ctx, tx, sessionID, OutputMessagePersistent, text, nil, "", time.Now().UTC())

	return err
}
