package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type queryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

const recoverableInputQuery = `
	WITH pending AS (
		SELECT session_inbox.session_id, MIN(session_inbox.id) AS first_input_id
		FROM session_inbox
		JOIN sessions ON sessions.id = session_inbox.session_id
		WHERE session_inbox.state = 'pending'
			AND sessions.killed_at IS NULL
			AND sessions.status NOT IN ('stopping', 'stopped', 'terminating', 'killed')
		GROUP BY session_inbox.session_id
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
	var resolvedAt sql.NullTime
	var reason sql.NullString
	var acceptedMessageID sql.NullInt64

	err := sc.Scan(
		&input.ID,
		&input.SessionID,
		&source,
		&input.RawContent,
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
) (*StoredMessage, error) {
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

	return &StoredMessage{
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
			AND status NOT IN ('stopping', 'stopped', 'terminating', 'killed')`,
		now, sessionID,
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

func loadMessage(ctx context.Context, q queryer, messageID int64) (*StoredMessage, error) {
	var msg StoredMessage
	var toolCallID, toolName, toolCallsRaw, reasoningContent, reasoningRaw, usageRaw sql.NullString
	var compactedAt, clearedAt sql.NullTime
	var costUSD sql.NullFloat64

	err := q.QueryRowContext(ctx, `
		SELECT id, session_id, role, content, tool_call_id, tool_name, tool_calls,
			reasoning_content, reasoning_raw, cost_usd, usage, compacted_at, cleared_at, created_at
		FROM messages WHERE id = ?`, messageID,
	).Scan(
		&msg.ID, &msg.SessionID, &msg.Role, &msg.Content,
		&toolCallID, &toolName, &toolCallsRaw, &reasoningContent, &reasoningRaw,
		&costUSD, &usageRaw, &compactedAt, &clearedAt, &msg.CreatedAt,
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

	if clearedAt.Valid {
		msg.ClearedAt = &clearedAt.Time
	}

	if toolCallsRaw.Valid {
		msg.ToolCalls = []byte(toolCallsRaw.String)
	}

	if reasoningRaw.Valid {
		msg.ReasoningRaw = []byte(reasoningRaw.String)
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
