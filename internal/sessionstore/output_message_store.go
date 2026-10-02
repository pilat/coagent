package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func (s *store) EnqueueOutput(ctx context.Context, draft OutputDraft) (*OutputCommit, error) {
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

//nolint:dupl // EnqueueOutput and EnqueueProgressOutput differ only in their eligibility gate.
func insertOutputTx(ctx context.Context, tx *sql.Tx, draft OutputDraft, mode CommitMode) (*OutputCommit, error) {
	owner, err := outputOwner(ctx, tx, draft.SessionID)
	if errors.Is(err, ErrOutputOwner) || errors.Is(err, ErrOutputNotRoot) {
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
	draft.Fingerprint = outputFingerprintWithRelease(draft.Type, draft.Content, draft.SessionID, draft.Attributes, draft.ReleasesInput)

	attributes := cloneAttributes(draft.Attributes)
	attributes[managerIDAttribute] = owner

	if isMessageOutput(draft.Type) {
		attributes, err = stampMessageOutputAttributes(ctx, tx, draft.SessionID, owner, draft.Attributes)
		if err != nil {
			return nil, err
		}
	}

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
			(session_id, type, content, attributes, source_key, fingerprint, created_at, releases_input)
		VALUES (?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?)`,
		draft.SessionID, draft.Type, draft.Content, string(encoded), draft.SourceKey, draft.Fingerprint, now,
		draft.ReleasesInput)
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

	var existingID int64
	var existingFingerprint string

	err = tx.QueryRowContext(ctx, `SELECT id, fingerprint FROM session_outbox WHERE session_id = ? AND source_key = ?`, draft.SessionID, draft.SourceKey).
		Scan(&existingID, &existingFingerprint)
	if err != nil {
		return nil, fmt.Errorf("load existing output: %w", err)
	}

	if existingFingerprint != draft.Fingerprint {
		return nil, fmt.Errorf("%w: session %d key %q", ErrOutputConflict, draft.SessionID, draft.SourceKey)
	}

	return &OutputCommit{OutputID: existingID, OwnerID: owner, Existing: true, Content: draft.Content}, nil
}

//nolint:wsl_v5 // Identity lookup keeps sentinel handling adjacent.
func (s *store) OutputBySourceKey(
	ctx context.Context,
	sessionID int64,
	sourceKey string,
) (*OutputRecord, error) {
	record, err := scanOutputRecord(s.db.QueryRowContext(ctx, `SELECT `+outputColumns+
		` FROM session_outbox WHERE session_id = ? AND source_key = ?`, sessionID, sourceKey))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoOutput
	}
	if err != nil {
		return nil, fmt.Errorf("load output by source key: %w", err)
	}

	return record, nil
}
