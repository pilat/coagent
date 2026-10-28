package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// AssistantAgentType is the agent type of a manager's assistant root.
const AssistantAgentType = "assistant"

// EnsureAssistantRoot preserves one durable desk identity per manager.
func (s *Store) EnsureAssistantRoot(
	ctx context.Context,
	projectID int64,
	owner, workDir, model, level string,
) (*SessionRecord, error) {
	if projectID <= 0 || owner == "" || workDir == "" || model == "" {
		return nil, errors.New("assistant root requires project, owner, work dir and model")
	}

	var record *SessionRecord

	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		var err error

		record, err = scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions
			WHERE agent_type = '`+AssistantAgentType+`' AND parent_id = 0 AND json_extract(attributes, '$.manager_id') = ?`, owner))
		if err == nil {
			return initializeAssistantModelTx(ctx, tx, record, model, level)
		}

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
			VALUES (?, ?, ?, ?, '`+AssistantAgentType+`', 'completed', ?, ?)`, projectID, model, level, string(attrs), now, now)
		if err != nil {
			return fmt.Errorf("insert assistant root: %w", err)
		}

		id, err := result.LastInsertId()
		if err != nil {
			return fmt.Errorf("assistant root id: %w", err)
		}

		if _, err := insertLifecycleOutput(ctx, tx, id, OutputSessionOpened,
			map[string]any{outputAttributeName: "Assistant", outputAttributeWorkDir: workDir},
			fmt.Sprintf("session:%d:opened", id), now); err != nil {
			return err
		}

		record, err = scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id))

		return err
	})

	return record, err
}

func initializeAssistantModelTx(ctx context.Context, tx *sql.Tx, record *SessionRecord, model, level string) error {
	if record.Model != "" {
		return nil
	}

	if _, err := tx.ExecContext(
		ctx,
		`UPDATE sessions SET model = ?, reasoning_level = ? WHERE id = ?`,
		model,
		level,
		record.ID,
	); err != nil {
		return fmt.Errorf("initialize assistant model: %w", err)
	}

	record.Model, record.ReasoningLevel = model, level

	return nil
}
