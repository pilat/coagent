package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func (s *store) CreateManagerRoot(
	ctx context.Context,
	create ManagerRootCreate,
) (*SessionRecord, *OutputCommit, error) {
	owner, err := managerOwner(create.Attributes)
	if err != nil {
		return nil, nil, err
	}

	if create.ProjectID <= 0 || create.Name == "" || create.WorkDir == "" {
		return nil, nil, errors.New("manager root requires project, name, and work dir")
	}

	if create.ReasoningLevel == "" {
		create.ReasoningLevel = defaultReasoningLevel
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin manager root: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()

	record, err := insertManagerRoot(ctx, tx, create, now)
	if err != nil {
		return nil, nil, err
	}

	commit, err := insertLifecycleOutput(ctx, tx, record.ID, OutputSessionOpened, "", owner,
		map[string]any{outputAttributeName: create.Name, outputAttributeWorkDir: create.WorkDir},
		fmt.Sprintf("session:%d:opened", record.ID), now)
	if err != nil {
		return nil, nil, err
	}

	if create.Prompt != "" {
		if err := insertManagerInput(ctx, tx, record.ID, create.Prompt, owner, now); err != nil {
			return nil, nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit manager root: %w", err)
	}

	return record, commit, nil
}

func (s *store) ReplaceManagerRoot(
	ctx context.Context,
	oldSessionID int64,
	name, workDir string,
) (*SessionRecord, *OutputCommit, error) {
	return s.replaceManagerRoot(ctx, oldSessionID, 0, name, workDir)
}

func (s *store) ReplaceManagerRootForInput(
	ctx context.Context,
	oldSessionID, inputID int64,
	name, workDir string,
) (*SessionRecord, *OutputCommit, error) {
	if inputID <= 0 {
		return nil, nil, errors.New("manager root replacement requires command input")
	}

	return s.replaceManagerRoot(ctx, oldSessionID, inputID, name, workDir)
}

//nolint:funlen // Replacement preserves a single transaction across old root, new root, and lifecycle output.
func (s *store) replaceManagerRoot(
	ctx context.Context,
	oldSessionID, inputID int64,
	name, workDir string,
) (*SessionRecord, *OutputCommit, error) {
	if oldSessionID <= 0 || name == "" || workDir == "" {
		return nil, nil, errors.New("manager root replacement requires session, name, and work dir")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin manager root replacement: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	old, err := scanSession(
		tx.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, oldSessionID),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("load replacement root: %w", err)
	}

	owner, err := managerOwner(old.Attributes)
	if err != nil {
		return nil, nil, err
	}

	if old.ParentID != 0 || old.KilledAt != nil || old.Status == SessionStatusTerminating ||
		old.Status == SessionStatusKilled {
		return nil, nil, errors.New("session cannot be replaced")
	}

	now := time.Now().UTC()

	result, err := tx.ExecContext(ctx, `
		UPDATE sessions SET status = 'terminating', updated_at = ?
		WHERE id = ? AND killed_at IS NULL AND status NOT IN ('terminating', 'killed')`, now, oldSessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("mark replacement root terminating: %w", err)
	}

	if err := requireOneSessionUpdate(result, oldSessionID); err != nil {
		return nil, nil, err
	}

	if inputID > 0 {
		if err := handleReplacementInput(ctx, tx, inputID, oldSessionID, now); err != nil {
			return nil, nil, err
		}
	}

	newRecord, err := insertManagerRoot(ctx, tx, ManagerRootCreate{
		ProjectID: old.ProjectID, Model: old.Model, ReasoningLevel: old.ReasoningLevel, Attributes: old.Attributes,
	}, now)
	if err != nil {
		return nil, nil, err
	}

	attrs := map[string]any{
		"old_session_id":       oldSessionID,
		"new_session_id":       newRecord.ID,
		outputAttributeName:    name,
		outputAttributeWorkDir: workDir,
	}

	commit, err := insertLifecycleOutput(ctx, tx, newRecord.ID, OutputSessionReplaced, "", owner, attrs,
		fmt.Sprintf("session:%d:replaced:%d", oldSessionID, newRecord.ID), now)
	if err != nil {
		return nil, nil, err
	}

	if inputID > 0 {
		if err := insertClearNotice(ctx, tx, newRecord.ID, inputID, owner, now); err != nil {
			return nil, nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit manager root replacement: %w", err)
	}

	return newRecord, commit, nil
}

func handleReplacementInput(ctx context.Context, tx *sql.Tx, inputID, sessionID int64, now time.Time) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE session_inbox SET state = 'handled', resolved_at = ?, resolution_reason = 'clear'
		WHERE id = ? AND session_id = ? AND state = 'pending'`, now, inputID, sessionID)
	if err != nil {
		return fmt.Errorf("handle clear input: %w", err)
	}

	return requireOnePendingResolution(ctx, tx, result, inputID)
}

func insertClearNotice(ctx context.Context, tx *sql.Tx, sessionID, inputID int64, owner string, now time.Time) error {
	attrs, err := json.Marshal(map[string]any{managerIDAttribute: owner})
	if err != nil {
		return fmt.Errorf("marshal clear output attributes: %w", err)
	}

	content := "Session cleared."
	key := fmt.Sprintf("input:%d:clear:result", inputID)

	fingerprint := outputFingerprint(OutputMessagePersistent, content, sessionID, nil)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO session_outbox (session_id, type, content, attributes, source_key, fingerprint, created_at)
		VALUES (?, 'message_persistent', ?, ?, ?, ?, ?)`, sessionID, content, string(attrs), key, fingerprint, now); err != nil {
		return fmt.Errorf("insert clear output: %w", err)
	}

	return nil
}

func managerOwner(attrs map[string]any) (string, error) {
	owner, _ := attrs[managerIDAttribute].(string)
	if owner == "" {
		return "", ErrOutputOwner
	}

	return owner, nil
}

func insertManagerRoot(
	ctx context.Context,
	tx *sql.Tx,
	create ManagerRootCreate,
	now time.Time,
) (*SessionRecord, error) {
	attrs, err := json.Marshal(create.Attributes)
	if err != nil {
		return nil, fmt.Errorf("marshal manager root attributes: %w", err)
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (project_id, model, reasoning_level, attributes, agent_type, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		create.ProjectID, create.Model, create.ReasoningLevel, string(attrs), rootAgentType, now, now)
	if err != nil {
		return nil, fmt.Errorf("insert manager root: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("manager root id: %w", err)
	}

	return &SessionRecord{
		ID: id, ProjectID: create.ProjectID, Model: create.Model, ReasoningLevel: create.ReasoningLevel,
		AgentType: rootAgentType, Status: SessionStatusActive, Attributes: cloneAttributes(create.Attributes),
		CreatedAt: now, UpdatedAt: now,
	}, nil
}

func insertManagerInput(
	ctx context.Context,
	tx *sql.Tx,
	sessionID int64,
	prompt, owner string,
	now time.Time,
) error {
	attrs, err := json.Marshal(map[string]any{managerIDAttribute: owner})
	if err != nil {
		return fmt.Errorf("marshal manager input attributes: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO session_inbox (session_id, source, raw_content, attributes, received_at)
		VALUES (?, 'user', ?, ?, ?)`, sessionID, prompt, string(attrs), now); err != nil {
		return fmt.Errorf("insert manager root input: %w", err)
	}

	return nil
}

func insertLifecycleOutput(
	ctx context.Context,
	tx *sql.Tx,
	sessionID int64,
	kind OutputType,
	content, owner string,
	attrs map[string]any,
	key string,
	now time.Time,
) (*OutputCommit, error) {
	fingerprint := outputFingerprint(kind, content, sessionID, attrs)
	encodedAttrs := cloneAttributes(attrs)
	encodedAttrs[managerIDAttribute] = owner

	encoded, err := json.Marshal(encodedAttrs)
	if err != nil {
		return nil, fmt.Errorf("marshal lifecycle output attributes: %w", err)
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO session_outbox (session_id, type, content, attributes, source_key, fingerprint, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`, sessionID, kind, content, string(encoded), key, fingerprint, now)
	if err != nil {
		return nil, fmt.Errorf("insert lifecycle output: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("lifecycle output id: %w", err)
	}

	return &OutputCommit{OutputID: id, OwnerID: owner}, nil
}
