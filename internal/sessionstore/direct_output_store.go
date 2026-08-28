//nolint:wrapcheck // SQL identity errors are wrapped by the transaction boundary.; nosemgrep: semgrep.coagent-no-preamble-before-package
package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	maxDirectMessages     = 4
	maxDirectMessageBytes = 16 * 1024
	maxDirectTotalBytes   = 32 * 1024
)

type DirectOutputStore interface {
	InsertToolResultWithDirectOutput(
		ctx context.Context,
		sessionID int64,
		message *StoredMessage,
		directMessages []string,
	) (messageID int64, outputs []*OutputCommit, err error)
}

var _ DirectOutputStore = (*store)(nil)

func (s *store) InsertToolResultWithDirectOutput(
	ctx context.Context,
	sessionID int64,
	message *StoredMessage,
	directMessages []string,
) (int64, []*OutputCommit, error) {
	if err := validateDirectOutput(message, directMessages); err != nil {
		return 0, nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, nil, fmt.Errorf("begin direct tool output: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	owner, err := outputOwner(ctx, tx, sessionID)
	if errors.Is(err, ErrOutputOwner) || errors.Is(err, ErrOutputNotRoot) {
		directMessages = nil
	} else if err != nil {
		return 0, nil, err
	}

	messageID, err := insertToolResultOnce(ctx, tx, sessionID, message)
	if err != nil {
		return 0, nil, err
	}

	outputs := make([]*OutputCommit, 0, len(directMessages))
	for i, content := range directMessages {
		commit, insertErr := insertDirectOutput(ctx, tx, sessionID, owner, message.ToolCallID, i, content)
		if insertErr != nil {
			return 0, nil, insertErr
		}

		outputs = append(outputs, commit)
	}

	if err := tx.Commit(); err != nil {
		return 0, nil, fmt.Errorf("commit direct tool output: %w", err)
	}

	return messageID, outputs, nil
}

func validateDirectOutput(message *StoredMessage, direct []string) error {
	if message == nil || message.Role != "tool" || message.ToolCallID == "" || message.ToolName == "" {
		return errors.New("invalid direct-output tool result")
	}

	if len(direct) > maxDirectMessages {
		return fmt.Errorf("direct output has %d messages; maximum is %d", len(direct), maxDirectMessages)
	}

	total := 0

	for _, content := range direct {
		if content == "" || len(content) > maxDirectMessageBytes {
			return errors.New("direct output contains an empty or oversized message")
		}

		total += len(content)
	}

	if total > maxDirectTotalBytes {
		return errors.New("direct output exceeds total size limit")
	}

	return nil
}

func insertToolResultOnce(
	ctx context.Context,
	tx *sql.Tx,
	sessionID int64,
	message *StoredMessage,
) (int64, error) {
	var existingID int64
	var existingContent string

	err := tx.QueryRowContext(ctx, `SELECT id, content FROM messages
		WHERE session_id = ? AND role = 'tool' AND tool_call_id = ? ORDER BY id LIMIT 1`,
		sessionID, message.ToolCallID).Scan(&existingID, &existingContent)
	if err == nil {
		if existingContent != message.Content {
			return 0, ErrOutputConflict
		}

		return existingID, nil
	}

	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("load direct-output tool result: %w", err)
	}

	result, err := tx.ExecContext(ctx, `INSERT INTO messages
		(session_id, role, content, tool_call_id, tool_name, created_at)
		VALUES (?, 'tool', ?, ?, ?, ?)`,
		sessionID, message.Content, message.ToolCallID, message.ToolName, message.CreatedAt)
	if err != nil {
		return 0, fmt.Errorf("insert direct-output tool result: %w", err)
	}

	return result.LastInsertId()
}

func insertDirectOutput(
	ctx context.Context,
	tx *sql.Tx,
	sessionID int64,
	owner, callID string,
	index int,
	content string,
) (*OutputCommit, error) {
	attributes, err := json.Marshal(map[string]any{managerIDAttribute: owner})
	if err != nil {
		return nil, fmt.Errorf("marshal direct output attributes: %w", err)
	}

	sourceKey := fmt.Sprintf("tool:%s:direct:%d", callID, index)
	fingerprint := OutputFingerprint(OutputMessagePersistent, content, sessionID, nil)

	result, err := tx.ExecContext(ctx, `INSERT INTO session_outbox
		(session_id, type, content, attributes, source_key, fingerprint, created_at)
		VALUES (?, 'message_persistent', ?, ?, ?, ?, ?)`,
		sessionID, content, string(attributes), sourceKey, fingerprint, time.Now().UTC())
	if err == nil {
		id, idErr := result.LastInsertId()
		return &OutputCommit{OutputID: id, OwnerID: owner}, idErr
	}

	if !isUniqueConstraintError(err) {
		return nil, fmt.Errorf("insert direct output: %w", err)
	}

	var id int64

	var existing string
	if err := tx.QueryRowContext(ctx, `SELECT id, fingerprint FROM session_outbox
		WHERE session_id = ? AND source_key = ?`, sessionID, sourceKey).Scan(&id, &existing); err != nil {
		return nil, fmt.Errorf("load direct output retry: %w", err)
	}

	if existing != fingerprint {
		return nil, ErrOutputConflict
	}

	return &OutputCommit{OutputID: id, OwnerID: owner, Existing: true}, nil
}
