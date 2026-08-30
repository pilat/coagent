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

	terminal := outputType == OutputMessagePersistent && !storedMessageHasToolCalls(message)

	phase := "progress"
	if terminal {
		phase = "final"
	} else if outputType == OutputMessagePersistent {
		phase = "reply"
	}

	key := fmt.Sprintf("message:%d:%s", messageID, phase)
	releasesInput := terminal
	fingerprint := outputFingerprintWithRelease(outputType, content, sessionID, nil, releasesInput)

	attributes, err := stampMessageOutputAttributes(ctx, tx, sessionID, owner, nil)
	if err != nil {
		return 0, nil, err
	}

	encoded, err := json.Marshal(attributes)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal assistant output attributes: %w", err)
	}

	result, err := tx.ExecContext(ctx, `
		INSERT INTO session_outbox
			(session_id, type, content, attributes, source_key, fingerprint, created_at, releases_input)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, outputType, content, string(encoded), key, fingerprint, time.Now().UTC(), releasesInput)
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

func storedMessageHasToolCalls(message *StoredMessage) bool {
	var calls []json.RawMessage

	return json.Unmarshal(message.ToolCalls, &calls) == nil && len(calls) > 0
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

	commit, err := enqueueOutputTx(ctx, tx, draft)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit output: %w", err)
	}

	return commit, nil
}

//nolint:dupl // EnqueueOutput and EnqueueProgressOutput differ only in their eligibility gate.
func enqueueOutputTx(ctx context.Context, tx *sql.Tx, draft OutputDraft) (*OutputCommit, error) {
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

	if isMessageOutput(draft.Type) {
		attributes, err = stampMessageOutputAttributes(ctx, tx, draft.SessionID, owner, draft.Attributes)
		if err != nil {
			return nil, err
		}
	}

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
