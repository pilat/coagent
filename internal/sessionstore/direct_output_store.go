//nolint:wrapcheck // SQL identity errors are wrapped by the transaction boundary.; nosemgrep: semgrep.coagent-no-preamble-before-package
package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/transcript"
)

const (
	// Caps one tool-result row's direct output; batch aggregation must fit
	// the same budget because the combined result is a single row.
	MaxDirectMessages     = 4
	MaxDirectMessageBytes = 16 * 1024
	MaxDirectTotalBytes   = 32 * 1024
)

type DirectOutputStore interface {
	InsertToolResultWithDirectOutput(
		ctx context.Context,
		sessionID int64,
		message *transcript.Message,
		directMessages []string,
	) (messageID int64, outputs []*OutputCommit, err error)

	// InsertToolResultSetOnce commits the complete decided result set for one
	// assistant turn — tool result rows plus their direct outputs — in a single
	// transaction, so a crash can never expose a failure row without the skips
	// it decided. Idempotent by call ID; row ids and output commits return in
	// call order.
	InsertToolResultSetOnce(
		ctx context.Context,
		sessionID int64,
		entries []ToolResultEntry,
	) ([]int64, [][]*OutputCommit, error)
}

// ToolResultEntry is one decided tool result with its direct outputs.
type ToolResultEntry struct {
	Message        *transcript.Message
	DirectMessages []string
}

var _ DirectOutputStore = (*store)(nil)

func (s *store) InsertToolResultSetOnce(
	ctx context.Context,
	sessionID int64,
	entries []ToolResultEntry,
) ([]int64, [][]*OutputCommit, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin tool result set: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	owner, err := outputOwner(ctx, tx, sessionID)
	if errors.Is(err, ErrOutputOwner) || errors.Is(err, ErrOutputNotRoot) {
		// The same degrade rule as the single-result path: results still
		// settle, their direct outputs do not.
		owner = ""
		entries = dropDirectMessages(entries)
	} else if err != nil {
		return nil, nil, err
	}

	if hasDirectMessages(entries) {
		// Fail closed behind a stop/kill fence: no late direct output may
		// appear below the stop result, even when its tool result settles.
		if err := outputSessionWritable(ctx, tx, sessionID); err != nil {
			return nil, nil, err
		}
	}

	ids := make([]int64, len(entries))
	outputs := make([][]*OutputCommit, len(entries))

	for i, entry := range entries {
		messageID, insertErr := insertToolResultOnce(ctx, tx, sessionID, entry.Message)
		if insertErr != nil {
			return nil, nil, insertErr
		}

		ids[i] = messageID

		// Direct-output validation is a property of the outputs, not the row:
		// results without direct messages settle as plain rows.
		if len(entry.DirectMessages) == 0 {
			outputs[i] = nil

			continue
		}

		if err := validateDirectOutput(entry.Message, entry.DirectMessages); err != nil {
			return nil, nil, err
		}

		commits := make([]*OutputCommit, 0, len(entry.DirectMessages))
		for j, content := range entry.DirectMessages {
			commit, directErr := insertDirectOutput(ctx, tx, sessionID, owner, entry.Message.ToolCallID, j, content)
			if directErr != nil {
				return nil, nil, directErr
			}

			commits = append(commits, commit)
		}

		outputs[i] = commits
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit tool result set: %w", err)
	}

	return ids, outputs, nil
}

func dropDirectMessages(entries []ToolResultEntry) []ToolResultEntry {
	stripped := make([]ToolResultEntry, len(entries))
	for i, entry := range entries {
		stripped[i] = ToolResultEntry{Message: entry.Message}
	}

	return stripped
}

func hasDirectMessages(entries []ToolResultEntry) bool {
	for _, entry := range entries {
		if len(entry.DirectMessages) > 0 {
			return true
		}
	}

	return false
}

func (s *store) InsertToolResultWithDirectOutput(
	ctx context.Context,
	sessionID int64,
	message *transcript.Message,
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

	// Fail closed behind a stop/kill fence: no late direct output may appear
	// below the stop result, even when its tool result still settles.
	if len(directMessages) > 0 {
		if err := outputSessionWritable(ctx, tx, sessionID); err != nil {
			return 0, nil, err
		}
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

func validateDirectOutput(message *transcript.Message, direct []string) error {
	if message == nil || message.Role != "tool" || message.ToolCallID == "" || message.ToolName == "" {
		return errors.New("invalid direct-output tool result")
	}

	if len(direct) > MaxDirectMessages {
		return fmt.Errorf("direct output has %d messages; maximum is %d", len(direct), MaxDirectMessages)
	}

	total := 0

	for _, content := range direct {
		if content == "" || len(content) > MaxDirectMessageBytes {
			return errors.New("direct output contains an empty or oversized message")
		}

		total += len(content)
	}

	if total > MaxDirectTotalBytes {
		return errors.New("direct output exceeds total size limit")
	}

	return nil
}

func insertToolResultOnce(
	ctx context.Context,
	tx *sql.Tx,
	sessionID int64,
	message *transcript.Message,
) (int64, error) {
	var existingID int64
	var existingContent string
	var existingToolError bool
	var existingToolName string

	err := tx.QueryRowContext(ctx, `SELECT id, content, tool_error, tool_name FROM messages
		WHERE session_id = ? AND role = 'tool' AND tool_call_id = ? ORDER BY id LIMIT 1`,
		sessionID, message.ToolCallID).Scan(&existingID, &existingContent, &existingToolError, &existingToolName)
	if err == nil {
		if existingContent != message.Content || existingToolError != message.ToolError ||
			existingToolName != message.ToolName {
			return 0, ErrOutputConflict
		}

		return existingID, nil
	}

	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("load direct-output tool result: %w", err)
	}

	result, err := tx.ExecContext(ctx, `INSERT INTO messages
		(session_id, role, content, tool_call_id, tool_name, tool_error, created_at)
		VALUES (?, 'tool', ?, ?, ?, ?, ?)`,
		sessionID, message.Content, message.ToolCallID, message.ToolName, message.ToolError, message.CreatedAt)
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
	attributes, err := stampMessageOutputAttributes(ctx, tx, sessionID, owner, nil)
	if err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(attributes)
	if err != nil {
		return nil, fmt.Errorf("marshal direct output attributes: %w", err)
	}

	sourceKey := fmt.Sprintf("tool:%s:direct:%d", callID, index)
	fingerprint := OutputFingerprint(OutputMessagePersistent, content, sessionID, nil)

	result, err := tx.ExecContext(ctx, `INSERT INTO session_outbox
		(session_id, type, content, attributes, source_key, fingerprint, created_at)
		VALUES (?, 'message_persistent', ?, ?, ?, ?, ?)`,
		sessionID, content, string(encoded), sourceKey, fingerprint, time.Now().UTC())
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
