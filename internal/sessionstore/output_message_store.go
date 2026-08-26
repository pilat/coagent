package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

//nolint:nonamedreturns // message and output identities need named results.
func (s *store) InsertAssistantMessageWithOutput(
	ctx context.Context,
	sessionID int64,
	message *StoredMessage,
	outputType OutputType,
	content string,
) (messageID int64, output *OutputCommit, err error) {
	if message == nil || message.Role != "assistant" || !isMessageOutput(outputType) || content == "" {
		return 0, nil, errors.New("invalid assistant output")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("begin assistant output: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	owner, err := outputOwner(ctx, tx, sessionID)
	if err != nil {
		return 0, nil, err
	}

	if err := outputSessionWritable(ctx, tx, sessionID); err != nil {
		return 0, nil, err
	}

	messageID, err = insertMessageWith(ctx, tx, sessionID, message)
	if err != nil {
		return 0, nil, err
	}

	phase := "progress"
	if outputType == OutputMessagePersistent {
		phase = "final"
	}

	key := fmt.Sprintf("message:%d:%s", messageID, phase)
	fingerprint := outputFingerprint(outputType, content, sessionID, nil)

	attributes, err := json.Marshal(map[string]any{managerIDAttribute: owner})
	if err != nil {
		return 0, nil, fmt.Errorf("marshal assistant output attributes: %w", err)
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO session_outbox
			(session_id, type, content, attributes, source_key, fingerprint, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sessionID, outputType, content, string(attributes), key, fingerprint, time.Now().UTC())
	if err != nil {
		return 0, nil, fmt.Errorf("insert assistant output: %w", err)
	}

	outputID, err := result.LastInsertId()
	if err != nil {
		return 0, nil, fmt.Errorf("assistant output id: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, nil, fmt.Errorf("commit assistant output: %w", err)
	}

	return messageID, &OutputCommit{OutputID: outputID, OwnerID: owner}, nil
}

func (s *store) EnqueueOutput(ctx context.Context, draft OutputDraft) (*OutputCommit, error) {
	if err := validateOutputDraft(draft); err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin enqueue output: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	owner, err := outputOwner(ctx, tx, draft.SessionID)
	if err != nil {
		return nil, err
	}

	if err := outputSessionWritable(ctx, tx, draft.SessionID); err != nil {
		return nil, err
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

	result, err := tx.ExecContext(ctx, `
		INSERT INTO session_outbox
			(session_id, type, content, attributes, source_key, fingerprint, created_at)
		VALUES (?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?)`,
		draft.SessionID, draft.Type, draft.Content, string(encoded), draft.SourceKey, draft.Fingerprint, now)
	if err == nil {
		id, idErr := result.LastInsertId()
		if idErr != nil {
			return nil, fmt.Errorf("output id: %w", idErr)
		}

		if commitErr := tx.Commit(); commitErr != nil {
			return nil, fmt.Errorf("commit output: %w", commitErr)
		}

		return &OutputCommit{OutputID: id, OwnerID: owner}, nil
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

	return &OutputCommit{OutputID: existingID, OwnerID: owner, Existing: true}, nil
}

// HandleInputWithOutput commits a command's terminal state with its response.
//
//nolint:funlen // A command's input state and output must remain visibly co-located in one transaction.
func (s *store) HandleInputWithOutput(
	ctx context.Context,
	inputID int64,
	reason string,
	draft OutputDraft,
) (*OutputCommit, error) {
	if reason == "" {
		return nil, errors.New("empty input handling reason")
	}

	if err := validateOutputDraft(draft); err != nil {
		return nil, err
	}

	if draft.SourceKey == "" {
		command := strings.Fields(reason)[0]
		draft.SourceKey = fmt.Sprintf("input:%d:%s:result", inputID, command)
		draft.Fingerprint = outputFingerprint(draft.Type, draft.Content, draft.SessionID, draft.Attributes)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin command output: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	input, err := loadInboxInput(ctx, tx, inputID)
	if err != nil {
		return nil, err
	}

	if input.State != InputStatePending {
		return nil, fmt.Errorf("%w: input %d is %s", ErrInputResolved, inputID, input.State)
	}

	if input.SessionID != draft.SessionID {
		return nil, errors.New("command output session does not match input")
	}

	owner, err := outputOwner(ctx, tx, draft.SessionID)
	if err != nil {
		return nil, err
	}

	attributes := cloneAttributes(draft.Attributes)
	attributes[managerIDAttribute] = owner

	encoded, err := json.Marshal(attributes)
	if err != nil {
		return nil, fmt.Errorf("marshal command output attributes: %w", err)
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE session_inbox
		SET state = 'handled', resolved_at = ?, resolution_reason = ?
		WHERE id = ? AND state = 'pending'`, time.Now().UTC(), reason, inputID)
	if err != nil {
		return nil, fmt.Errorf("handle command input %d: %w", inputID, err)
	}

	if err := requireOnePendingResolution(ctx, tx, result, inputID); err != nil {
		return nil, err
	}

	now := time.Now().UTC()

	result, err = tx.ExecContext(ctx, `
		INSERT INTO session_outbox
			(session_id, type, content, attributes, source_key, fingerprint, created_at)
		VALUES (?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?)`,
		draft.SessionID, draft.Type, draft.Content, string(encoded), draft.SourceKey, draft.Fingerprint, now)
	if err != nil {
		return nil, fmt.Errorf("insert command output: %w", err)
	}

	outputID, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("command output id: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit command output: %w", err)
	}

	return &OutputCommit{OutputID: outputID, OwnerID: owner}, nil
}
