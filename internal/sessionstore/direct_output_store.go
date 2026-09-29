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
	// InsertToolResultSetOnce commits the complete decided result set for one
	// assistant turn — tool result rows plus their direct outputs — in a single
	// transaction, so a crash can never expose a failure row without the skips
	// it decided. Idempotent by assistant call occurrence; row ids and output commits return in
	// call order.
	InsertToolResultSetOnce(
		ctx context.Context,
		sessionID int64,
		entries []ToolResultEntry,
	) ([]int64, [][]*OutputCommit, error)
}

// ToolResultEntry is one decided tool result with its direct outputs.
type ToolResultEntry struct {
	CallRef        CallRef
	Message        *transcript.Message
	DirectMessages []string
}

// CallRef identifies one call in a persisted assistant message.
type CallRef struct {
	AssistantMessageID int64
	Index              int
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

	transactionTime := time.Now().UTC()

	owner, err := outputOwner(ctx, tx, sessionID)
	if errors.Is(err, ErrOutputOwner) || errors.Is(err, ErrOutputNotRoot) {
		// Internal results still settle without a manager delivery target.
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
		messageID, fresh, insertErr := insertToolResultOnceAt(ctx, tx, sessionID, entry.CallRef, entry.Message, transactionTime)
		if insertErr != nil {
			return nil, nil, insertErr
		}

		ids[i] = messageID

		// A replayed row settles nothing new: it must not disturb a newer
		// check a later turn already opened.
		if fresh {
			// Fresh model-visible input invalidates any stale completion check
			// in the same commit that settles the result superseding it.
			if err := invalidateCompletionCheckTx(ctx, tx, sessionID); err != nil {
				return nil, nil, err
			}
		}

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
			key := fmt.Sprintf("tool:%d:%d:direct:%d", entry.CallRef.AssistantMessageID, entry.CallRef.Index, j)
			if !fresh {
				var legacy bool
				if err := tx.QueryRowContext(ctx, `SELECT tool_call_owner_id IS NULL FROM messages WHERE id = ?`, messageID).Scan(&legacy); err != nil {
					return nil, nil, fmt.Errorf("load tool result identity: %w", err)
				}
				if legacy {
					var legacyResults int
					if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages WHERE session_id = ?
						AND role = 'tool' AND tool_call_owner_id IS NULL AND tool_call_id = ?`,
						sessionID, entry.Message.ToolCallID).Scan(&legacyResults); err != nil {
						return nil, nil, fmt.Errorf("load legacy direct output identity: %w", err)
					}
					if legacyResults != 1 {
						return nil, nil, errors.New("ambiguous legacy direct output identity")
					}
					key = fmt.Sprintf("tool:%s:direct:%d", entry.Message.ToolCallID, j)
				}
			}
			commit, directErr := insertDirectOutputKey(ctx, tx, sessionID, owner, key, content)
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
		stripped[i] = ToolResultEntry{CallRef: entry.CallRef, Message: entry.Message}
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
	ref CallRef,
	message *transcript.Message,
) (int64, error) {
	messageID, _, err := insertToolResultOnceAt(ctx, tx, sessionID, ref, message, time.Now().UTC())

	return messageID, err
}

func insertToolResultOnceAt(
	ctx context.Context,
	tx *sql.Tx,
	sessionID int64,
	ref CallRef,
	message *transcript.Message,
	transactionTime time.Time,
) (int64, bool, error) {
	if err := validateCallRef(ctx, tx, sessionID, ref, message); err != nil {
		return 0, false, err
	}
	var existingID int64
	var existingContent string
	var existingToolError bool
	var existingToolName string
	var existingAttachments sql.NullString

	err := tx.QueryRowContext(ctx, `SELECT id, content, tool_error, tool_name, attachments FROM messages
		WHERE session_id = ? AND role = 'tool' AND tool_call_owner_id = ? AND tool_call_index = ?`,
		sessionID, ref.AssistantMessageID, ref.Index).Scan(
		&existingID, &existingContent, &existingToolError, &existingToolName, &existingAttachments,
	)
	if errors.Is(err, sql.ErrNoRows) {
		var legacyCount int
		var ownerCallCount int
		err = tx.QueryRowContext(ctx, `SELECT id, content, tool_error, tool_name, attachments, COUNT(*),
			(SELECT COUNT(*) FROM messages owner, json_each(owner.tool_calls) call
			WHERE owner.id = ? AND COALESCE(json_extract(call.value, '$.ID'), json_extract(call.value, '$.id')) = ?)
			FROM messages
			WHERE session_id = ? AND role = 'tool' AND tool_call_owner_id IS NULL AND tool_call_id = ?
			AND id > ? AND id < COALESCE((SELECT MIN(id) FROM messages WHERE session_id = ?
			AND role = 'assistant' AND id > ? AND json_array_length(tool_calls) > 0), 9223372036854775807)
			GROUP BY session_id`, ref.AssistantMessageID, message.ToolCallID, sessionID, message.ToolCallID, ref.AssistantMessageID,
			sessionID, ref.AssistantMessageID).Scan(
			&existingID, &existingContent, &existingToolError, &existingToolName, &existingAttachments, &legacyCount, &ownerCallCount)
		if err == nil && (legacyCount != 1 || ownerCallCount != 1) {
			return 0, false, errors.New("ambiguous legacy tool result identity")
		}
	}
	if err == nil {
		if existingContent != message.Content || existingToolError != message.ToolError ||
			existingToolName != message.ToolName ||
			toolResultAttachmentsIdentity(
				[]byte(existingAttachments.String),
			) != toolResultAttachmentsIdentity(
				message.Attachments,
			) {
			return 0, false, ErrOutputConflict
		}

		// A replayed row settles nothing new: the caller must not disturb a
		// newer check a later turn already opened.
		return existingID, false, nil
	}

	if !errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("load direct-output tool result: %w", err)
	}

	createdAt := message.CreatedAt
	if createdAt.IsZero() {
		createdAt = transactionTime
	}

	result, err := tx.ExecContext(ctx, `INSERT INTO messages
		(session_id, role, content, tool_call_id, tool_name, tool_error, attachments, created_at, tool_call_owner_id, tool_call_index)
		VALUES (?, 'tool', ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, message.Content, message.ToolCallID, message.ToolName, message.ToolError,
		nullRawJSON(message.Attachments), createdAt, ref.AssistantMessageID, ref.Index)
	if err != nil {
		return 0, false, fmt.Errorf("insert direct-output tool result: %w", err)
	}

	messageID, err := result.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("direct-output tool result id: %w", err)
	}

	return messageID, true, nil
}

func validateCallRef(ctx context.Context, tx *sql.Tx, sessionID int64, ref CallRef, message *transcript.Message) error {
	if message == nil || message.Role != "tool" || message.ToolCallID == "" || message.ToolName == "" ||
		ref.AssistantMessageID <= 0 || ref.Index < 0 {
		return errors.New("invalid tool result call reference")
	}

	var raw string
	if err := tx.QueryRowContext(ctx, `SELECT tool_calls FROM messages
		WHERE id = ? AND session_id = ? AND role = 'assistant'`, ref.AssistantMessageID, sessionID).Scan(&raw); err != nil {
		return fmt.Errorf("load tool call owner: %w", err)
	}

	var calls []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(raw), &calls); err != nil {
		return fmt.Errorf("decode tool call owner: %w", err)
	}
	if ref.Index >= len(calls) || calls[ref.Index].ID != message.ToolCallID || calls[ref.Index].Name != message.ToolName {
		return errors.New("tool result does not match its call reference")
	}

	return nil
}

func toolResultAttachmentsIdentity(raw json.RawMessage) string {
	var refs []json.RawMessage
	if len(raw) == 0 || (json.Unmarshal(raw, &refs) == nil && len(refs) == 0) {
		return ""
	}

	return string(raw)
}

func insertDirectOutputKey(
	ctx context.Context,
	tx *sql.Tx,
	sessionID int64,
	owner, sourceKey string,
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
