package sessionstore

import (
	"context"
	"database/sql"
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

func insertToolResultOnce(
	ctx context.Context,
	tx *sql.Tx,
	sessionID int64,
	message *transcript.Message,
) (int64, error) {
	messageID, _, err := insertToolResultOnceAt(ctx, tx, sessionID, message, time.Now().UTC())

	return messageID, err
}

func insertToolResultOnceAt(
	ctx context.Context,
	tx *sql.Tx,
	sessionID int64,
	message *transcript.Message,
	transactionTime time.Time,
) (int64, bool, error) {
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

	result, err := tx.ExecContext(
		ctx,
		`INSERT INTO messages
		(session_id, role, content, tool_call_id, tool_name, tool_error, attachments, created_at)
		VALUES (?, 'tool', ?, ?, ?, ?, ?, ?)`,
		sessionID,
		message.Content,
		message.ToolCallID,
		message.ToolName,
		message.ToolError,
		nullRawJSON(message.Attachments),
		createdAt,
	)
	if err != nil {
		return 0, false, fmt.Errorf("insert direct-output tool result: %w", err)
	}

	messageID, err := result.LastInsertId()
	if err != nil {
		return 0, false, fmt.Errorf("direct-output tool result id: %w", err)
	}

	return messageID, true, nil
}
