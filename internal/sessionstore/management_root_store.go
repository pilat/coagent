package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Attribute keys mirror controllerapi values; this package may not import it.
const (
	managementSurfaceAttribute = "management_surface"
	managementSurfaceValue     = "service-topic"
	telegramTopicAttribute     = "telegram_topic_id"
)

// EnsureManagementRoot returns the manager's one live management root in the
// project, inserting it with its session_opened lifecycle row when absent. A
// stale service-topic binding is patched to the current topic. Concurrent
// ensures collide on the partial unique index; the loser re-selects the winner
// inside the same transaction, so exactly one lifecycle row ever exists.
func (s *store) EnsureManagementRoot(
	ctx context.Context,
	projectID int64,
	owner string,
	topicID int64,
	name, workDir string,
) (*SessionRecord, *OutputCommit, error) {
	if projectID <= 0 || owner == "" || topicID <= 0 || name == "" || workDir == "" {
		return nil, nil, errors.New("management root ensure requires project, owner, topic, name, and work dir")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin management root ensure: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()

	record, err := selectManagementRoot(ctx, tx, projectID, owner)
	if err != nil {
		return nil, nil, err
	}

	if record != nil {
		if err := patchManagementTopic(ctx, tx, record, topicID, now); err != nil {
			return nil, nil, err
		}

		if err := tx.Commit(); err != nil {
			return nil, nil, fmt.Errorf("commit management root ensure: %w", err)
		}

		return record, nil, nil
	}

	record, commit, err := insertManagementRoot(ctx, tx, projectID, owner, topicID, name, workDir, now)
	if err == nil {
		if err := tx.Commit(); err != nil {
			return nil, nil, fmt.Errorf("commit management root ensure: %w", err)
		}

		return record, commit, nil
	}

	if !isManagementRootConflict(err) {
		return nil, nil, err
	}

	// A concurrent ensure won the unique index; the loser adopts the winner
	// inside the same transaction so only one lifecycle row ever exists.
	record, err = selectManagementRoot(ctx, tx, projectID, owner)
	if err != nil {
		return nil, nil, err
	}

	if record == nil {
		return nil, nil, errors.New("management root conflict without a winner")
	}

	if err := patchManagementTopic(ctx, tx, record, topicID, now); err != nil {
		return nil, nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit management root ensure: %w", err)
	}

	return record, nil, nil
}

func selectManagementRoot(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	owner string,
) (*SessionRecord, error) {
	record, err := scanSession(tx.QueryRowContext(ctx, `
		SELECT `+sessionColumns+` FROM sessions
		WHERE project_id = ? AND parent_id = 0 AND killed_at IS NULL
			AND status NOT IN ('terminating', 'killed')
			AND json_extract(attributes, '$.manager_id') = ?
			AND json_extract(attributes, '$.management_surface') IS NOT NULL
		LIMIT 1`, projectID, owner))
	if errors.Is(err, errSessionNotFound) {
		return nil, nil //nolint:nilnil // no root yet is the normal first-ensure state
	}

	if err != nil {
		return nil, fmt.Errorf("select management root: %w", err)
	}

	return record, nil
}

func insertManagementRoot(
	ctx context.Context,
	tx *sql.Tx,
	projectID int64,
	owner string,
	topicID int64,
	name, workDir string,
	now time.Time,
) (*SessionRecord, *OutputCommit, error) {
	attrs, err := json.Marshal(map[string]any{
		managerIDAttribute:         owner,
		managementSurfaceAttribute: managementSurfaceValue,
		telegramTopicAttribute:     topicID,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal management root attributes: %w", err)
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (project_id, model, reasoning_level, attributes, agent_type,
			created_at, updated_at)
		VALUES (?, '', ?, ?, ?, ?, ?)`,
		projectID, defaultReasoningLevel, string(attrs), rootAgentType, now, now)
	if err != nil {
		return nil, nil, fmt.Errorf("insert management root: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, nil, fmt.Errorf("management root id: %w", err)
	}

	commit, err := insertLifecycleOutput(ctx, tx, id, OutputSessionOpened, "", owner,
		map[string]any{outputAttributeName: name, outputAttributeWorkDir: workDir},
		fmt.Sprintf("session:%d:opened", id), now)
	if err != nil {
		return nil, nil, err
	}

	return &SessionRecord{
		ID: id, ProjectID: projectID, ReasoningLevel: defaultReasoningLevel,
		AgentType: rootAgentType, Status: SessionStatusActive,
		Attributes: map[string]any{
			managerIDAttribute:         owner,
			managementSurfaceAttribute: managementSurfaceValue,
			telegramTopicAttribute:     topicID,
		},
		CreatedAt: now, UpdatedAt: now,
	}, commit, nil
}

// patchManagementTopic rebinds a stale service-topic attribute to the current
// topic. It never writes a lifecycle row: the session_opened output predates
// the rebind and must stay single and stable.
func patchManagementTopic(
	ctx context.Context,
	tx *sql.Tx,
	record *SessionRecord,
	topicID int64,
	now time.Time,
) error {
	if topicNumber(record.Attributes[telegramTopicAttribute]) == topicID {
		return nil
	}

	record.Attributes[telegramTopicAttribute] = topicID

	encoded, err := json.Marshal(record.Attributes)
	if err != nil {
		return fmt.Errorf("marshal management root attributes: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET attributes = ?, updated_at = ? WHERE id = ?`,
		string(encoded), now, record.ID); err != nil {
		return fmt.Errorf("patch management root topic: %w", err)
	}

	record.UpdatedAt = now

	return nil
}

func isManagementRootConflict(err error) bool {
	return err != nil && strings.Contains(err.Error(), "idx_sessions_management_root")
}

// topicNumber reads the durable topic binding across JSON number shapes.
func topicNumber(raw any) int64 {
	switch v := raw.(type) {
	case int:
		return int64(v)
	case int64:
		return v
	case float64:
		return int64(v)
	case json.Number:
		id, err := v.Int64()
		if err != nil {
			return 0
		}

		return id
	default:
		return 0
	}
}
