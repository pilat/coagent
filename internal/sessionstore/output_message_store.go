package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func (s *Store) EnqueueOutput(ctx context.Context, draft OutputDraft) (*OutputCommit, error) {
	if err := validateOutputDraft(draft); err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin enqueue output: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	commit, err := insertOutputTx(ctx, tx, draft, CommitLoop)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit output: %w", err)
	}

	return commit, nil
}

func insertOutputTx(ctx context.Context, tx *sql.Tx, draft OutputDraft, mode CommitMode) (*OutputCommit, error) {
	owner, err := outputOwner(ctx, tx, draft.SessionID)
	if errors.Is(err, ErrOutputOwner) || errors.Is(err, ErrOutputNotRoot) {
		//nolint:nilnil // Ownerless sessions and subagents have no manager delivery obligations.
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	if mode == CommitLoop {
		if err := outputSessionWritable(ctx, tx, draft.SessionID); err != nil {
			return nil, err
		}
	}

	if err := validateLifecycleTarget(ctx, tx, draft, owner); err != nil {
		return nil, err
	}

	attributes := cloneAttributes(draft.Attributes)
	attributes[managerIDAttribute] = owner

	encoded, err := json.Marshal(attributes)
	if err != nil {
		return nil, fmt.Errorf("marshal output attributes: %w", err)
	}

	now := time.Now().UTC()
	if !draft.CreatedAt.IsZero() {
		now = draft.CreatedAt.UTC()
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO session_outbox
			(session_id, type, content, attributes, source_key, created_at)
		VALUES (?, ?, ?, ?, NULLIF(?, ''), ?)`,
		draft.SessionID, draft.Type, draft.Content, string(encoded), draft.SourceKey, now)
	if err == nil {
		id, idErr := result.LastInsertId()
		if idErr != nil {
			return nil, fmt.Errorf("output id: %w", idErr)
		}

		return &OutputCommit{OutputID: id, OwnerID: owner, Content: draft.Content}, nil
	}

	if draft.SourceKey == "" || !isUniqueConstraintError(err) {
		return nil, fmt.Errorf("insert output: %w", err)
	}

	return existingOutputTx(ctx, tx, draft, owner)
}

func existingOutputTx(ctx context.Context, tx *sql.Tx, draft OutputDraft, owner string) (*OutputCommit, error) {
	var existingID int64
	var content string

	err := tx.QueryRowContext(ctx, `SELECT id, content FROM session_outbox WHERE session_id = ? AND source_key = ?`, draft.SessionID, draft.SourceKey).
		Scan(&existingID, &content)
	if err != nil {
		return nil, fmt.Errorf("load existing output: %w", err)
	}

	return &OutputCommit{OutputID: existingID, OwnerID: owner, Existing: true, Content: content}, nil
}
