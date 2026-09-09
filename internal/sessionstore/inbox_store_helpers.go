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

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const recoverableInputQuery = `
	WITH candidate_input AS (
		SELECT id, session_id, source, state, trim(raw_content, char(
			9, 10, 11, 12, 13, 32, 133, 160, 5760,
			8192, 8193, 8194, 8195, 8196, 8197, 8198, 8199, 8200, 8201, 8202,
			8232, 8233, 8239, 8287, 12288
		)) AS content
		FROM session_inbox
	),
	pending AS (
		SELECT candidate_input.session_id, MIN(candidate_input.id) AS first_input_id
		FROM candidate_input
		JOIN sessions ON sessions.id = candidate_input.session_id
		WHERE candidate_input.state = 'pending'
			AND sessions.killed_at IS NULL
			AND sessions.status NOT IN ('stopping', 'terminating', 'killed')
			AND (sessions.status NOT IN ('stopped', 'error')
				OR EXISTS (
					SELECT 1 FROM candidate_input resumable
					WHERE resumable.session_id = sessions.id AND resumable.state = 'pending'
						AND (resumable.source = 'agent' OR (
							resumable.source = 'user'
							AND resumable.content NOT IN ('/status', '/help', '/schedules', '/compact')
							AND resumable.content NOT GLOB '/compact *'
						))
				)
				OR (sessions.status = 'stopped' AND candidate_input.source = 'user'
					AND (candidate_input.content IN ('/status', '/help', '/schedules', '/compact')
						OR candidate_input.content GLOB '/compact *')
					AND NOT EXISTS (
						SELECT 1 FROM candidate_input earlier
						WHERE earlier.session_id = sessions.id AND earlier.state = 'pending'
							AND earlier.id < candidate_input.id
					))
			)
		GROUP BY candidate_input.session_id
	),
	active_accepted AS (
		SELECT sessions.id AS session_id, MIN(session_inbox.id) AS first_input_id
		FROM sessions
		JOIN session_inbox ON session_inbox.session_id = sessions.id
		WHERE sessions.status = 'active'
			AND sessions.killed_at IS NULL
			AND session_inbox.state = 'accepted'
			AND session_inbox.accepted_message_id IS NOT NULL
		GROUP BY sessions.id
	)
	SELECT session_id
	FROM (
		SELECT 0 AS bucket, first_input_id AS sort_id, session_id
		FROM pending

		UNION ALL

		SELECT 1 AS bucket, first_input_id AS sort_id, session_id
		FROM active_accepted
		WHERE NOT EXISTS (
				SELECT 1 FROM pending WHERE pending.session_id = active_accepted.session_id
			)
	)
	ORDER BY bucket, sort_id, session_id`

const acceptedInputExistsQuery = `
	SELECT EXISTS (
		SELECT 1
		FROM session_inbox
		WHERE session_id = ?
			AND state = 'accepted'
			AND accepted_message_id IS NOT NULL
	)`

func loadInboxInput(ctx context.Context, q queryer, inputID int64) (*InboxInput, error) {
	input, err := scanInboxInput(q.QueryRowContext(
		ctx,
		`SELECT `+inboxColumns+` FROM session_inbox WHERE id = ?`,
		inputID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: input %d", ErrInputNotFound, inputID)
	}

	if err != nil {
		return nil, fmt.Errorf("load input %d: %w", inputID, err)
	}

	return input, nil
}

func scanInboxInput(sc rowScanner) (*InboxInput, error) {
	var input InboxInput
	var source, state string
	var attributes string
	var resolvedAt sql.NullTime
	var reason sql.NullString
	var acceptedMessageID sql.NullInt64

	err := sc.Scan(
		&input.ID,
		&input.SessionID,
		&source,
		&input.RawContent,
		&attributes,
		&input.ReceivedAt,
		&state,
		&resolvedAt,
		&reason,
		&acceptedMessageID,
	)
	if err != nil {
		return nil, fmt.Errorf("scan inbox input: %w", err)
	}

	input.Source = InputSource(source)

	input.State = InputState(state)
	if err := json.Unmarshal([]byte(attributes), &input.Attributes); err != nil {
		return nil, fmt.Errorf("decode inbox attributes: %w", err)
	}

	input.ResolutionReason = reason.String
	input.AcceptedMessageID = acceptedMessageID.Int64

	if resolvedAt.Valid {
		input.ResolvedAt = &resolvedAt.Time
	}

	return &input, nil
}

func insertPromotedMessage(
	ctx context.Context,
	tx *sql.Tx,
	input *InboxInput,
	preparedContent string,
) (*transcript.Message, error) {
	result, err := tx.ExecContext(ctx, `
		INSERT INTO messages (session_id, role, content, created_at)
		VALUES (?, 'user', ?, ?)`,
		input.SessionID, preparedContent, input.ReceivedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert promoted input %d: %w", input.ID, err)
	}

	messageID, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("promoted message id: %w", err)
	}

	return &transcript.Message{
		ID:        messageID,
		SessionID: input.SessionID,
		Role:      "user",
		Content:   preparedContent,
		CreatedAt: input.ReceivedAt,
	}, nil
}

func acceptPendingInput(
	ctx context.Context,
	tx *sql.Tx,
	inputID, messageID int64,
	now time.Time,
) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE session_inbox
		SET state = 'accepted', resolved_at = ?, accepted_message_id = ?
		WHERE id = ? AND state = 'pending'`,
		now, messageID, inputID,
	)
	if err != nil {
		return fmt.Errorf("accept input %d: %w", inputID, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("accept input rows affected: %w", err)
	}

	if affected != 1 {
		return fmt.Errorf("%w: input %d changed during promotion", ErrInputResolved, inputID)
	}

	return nil
}

func cancelPendingInputTree(
	ctx context.Context,
	tx *sql.Tx,
	sessionID int64,
	includeDescendants bool,
	now time.Time,
) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE session_inbox
		SET state = 'cancelled', resolved_at = ?, resolution_reason = 'killed'
		WHERE state = 'pending' AND session_id IN (
			SELECT id FROM sessions
			WHERE id = ? OR (? AND root_id = ?)
		)`,
		now, sessionID, includeDescendants, sessionID,
	)
	if err != nil {
		return fmt.Errorf("cancel killed session input: %w", err)
	}

	if _, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("cancel killed input rows affected: %w", err)
	}

	return nil
}

func activatePromotedInputSession(
	ctx context.Context,
	tx *sql.Tx,
	sessionID, inputID int64,
	now time.Time,
) error {
	result, err := tx.ExecContext(ctx, `
		UPDATE sessions
		SET status = 'active', updated_at = ?
		WHERE id = ? AND killed_at IS NULL
			AND status NOT IN ('stopping', 'terminating', 'killed')
			AND (status <> 'stopped' OR EXISTS (
				SELECT 1 FROM session_inbox resume
				WHERE resume.id = ? AND resume.session_id = sessions.id
					AND resume.source IN ('user', 'agent')
			))`,
		now, sessionID, inputID,
	)
	if err != nil {
		return fmt.Errorf("activate session %d for input %d: %w", sessionID, inputID, err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("activate session rows affected: %w", err)
	}

	if affected != 1 {
		return fmt.Errorf("%w: session %d", ErrSessionNotAcceptingInput, sessionID)
	}

	return nil
}

func loadMessage(ctx context.Context, q queryer, messageID int64) (*transcript.Message, error) {
	var msg transcript.Message
	var toolCallID, toolName, toolCallsRaw, reasoningContent, reasoningRaw, attachmentsRaw, usageRaw sql.NullString
	var compactedAt sql.NullTime
	var costUSD sql.NullFloat64

	err := q.QueryRowContext(ctx, `
		SELECT id, session_id, role, content, tool_call_id, tool_name, tool_error, tool_calls,
			reasoning_content, reasoning_raw, attachments, cost_usd, usage, compacted_at, created_at
		FROM messages WHERE id = ?`, messageID,
	).Scan(
		&msg.ID, &msg.SessionID, &msg.Role, &msg.Content,
		&toolCallID, &toolName, &msg.ToolError, &toolCallsRaw, &reasoningContent, &reasoningRaw,
		&attachmentsRaw,
		&costUSD, &usageRaw, &compactedAt, &msg.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("load accepted message %d: %w", messageID, err)
	}

	msg.ToolCallID = toolCallID.String
	msg.ToolName = toolName.String
	msg.ReasoningContent = reasoningContent.String
	msg.CostUSD = costUSD.Float64

	if compactedAt.Valid {
		msg.CompactedAt = &compactedAt.Time
	}

	if toolCallsRaw.Valid {
		msg.ToolCalls = []byte(toolCallsRaw.String)
	}

	if reasoningRaw.Valid {
		msg.ReasoningRaw = []byte(reasoningRaw.String)
	}

	if attachmentsRaw.Valid && attachmentsRaw.String != "" {
		msg.Attachments = []byte(attachmentsRaw.String)
	}

	if usageRaw.Valid {
		msg.Usage = []byte(usageRaw.String)
	}

	return &msg, nil
}

func scanSessionIDs(rows *sql.Rows, label string) ([]int64, error) {
	var sessionIDs []int64

	for rows.Next() {
		var sessionID int64

		if err := rows.Scan(&sessionID); err != nil {
			return nil, fmt.Errorf("scan %s session id: %w", label, err)
		}

		sessionIDs = append(sessionIDs, sessionID)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s session ids: %w", label, err)
	}

	return sessionIDs, nil
}

func requireOnePendingResolution(
	ctx context.Context,
	q queryer,
	result sql.Result,
	inputID int64,
) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("resolve input rows affected: %w", err)
	}

	if affected == 1 {
		return nil
	}

	input, err := loadInboxInput(ctx, q, inputID)
	if err != nil {
		return err
	}

	return fmt.Errorf("%w: input %d is %s", ErrInputResolved, inputID, input.State)
}
