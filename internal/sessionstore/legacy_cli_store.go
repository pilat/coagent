package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// LegacyCLIClaimStore claims the unambiguous pre-owner local-chat roots before
// daemon recovery can execute their accepted work without an outbox owner.
type LegacyCLIClaimStore interface {
	ClaimLegacyCLIRoots(ctx context.Context, projectName, projectDir, channel, managerID string) error
}

//nolint:funlen // Claiming ownership and lifecycle output together closes the pre-recovery gap.
func (s *store) ClaimLegacyCLIRoots(ctx context.Context, projectName, projectDir, channel, managerID string) error {
	if projectName == "" || projectDir == "" || channel == "" || managerID == "" {
		return errors.New("legacy cli claim requires project identity, channel, and manager")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin legacy cli claim: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
		SELECT sessions.id, sessions.attributes, projects.name, projects.work_dir
		FROM sessions JOIN projects ON projects.id = sessions.project_id
		WHERE projects.name = ? AND projects.work_dir = ?
			AND sessions.parent_id = 0 AND sessions.killed_at IS NULL
			AND sessions.status NOT IN ('terminating', 'killed')`, projectName, projectDir)
	if err != nil {
		return fmt.Errorf("list legacy cli roots: %w", err)
	}
	defer rows.Close()

	now := time.Now().UTC()

	for rows.Next() {
		var sessionID int64

		var encoded, name, workDir string
		if err := rows.Scan(&sessionID, &encoded, &name, &workDir); err != nil {
			return fmt.Errorf("scan legacy cli root: %w", err)
		}

		var attributes map[string]any
		if err := json.Unmarshal([]byte(encoded), &attributes); err != nil {
			return fmt.Errorf("decode legacy cli root %d: %w", sessionID, err)
		}

		owner, _ := attributes[managerIDAttribute].(string)
		if owner != "" || attributes["channel"] != channel {
			continue
		}

		attributes[managerIDAttribute] = managerID

		updated, err := json.Marshal(attributes)
		if err != nil {
			return fmt.Errorf("marshal legacy cli root %d: %w", sessionID, err)
		}

		result, err := tx.ExecContext(ctx, `UPDATE sessions SET attributes = ?, updated_at = ?
			WHERE id = ? AND (
				json_type(attributes, '$.manager_id') IS NULL OR
				json_type(attributes, '$.manager_id') <> 'text' OR
				json_extract(attributes, '$.manager_id') = ''
			)`, string(updated), now, sessionID)
		if err != nil {
			return fmt.Errorf("claim legacy cli root %d: %w", sessionID, err)
		}

		if err := requireOneSessionUpdate(result, sessionID); err != nil {
			return err
		}

		if _, err := insertLifecycleOutput(
			ctx,
			tx,
			sessionID,
			OutputSessionOpened,
			"",
			managerID,
			map[string]any{outputAttributeName: name, outputAttributeWorkDir: workDir},
			fmt.Sprintf("session:%d:opened", sessionID),
			now,
		); err != nil {
			return fmt.Errorf("insert legacy cli root %d lifecycle: %w", sessionID, err)
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate legacy cli roots: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit legacy cli claim: %w", err)
	}

	return nil
}
