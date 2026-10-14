package sessionstore

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/transcript"
)

// AssistantObservation is one instruction's user-bounded answer window.
type AssistantObservation struct {
	Input    *InboxInput
	Session  *SessionRecord
	NextUser int64
	Answer   string
	// Settled means the window ends in a final with no completion check pending: the
	// session finished its turn instead of pausing inside a tool call.
	Settled bool
}

// ObserveAssistantInputTx ignores process and agent inputs when bounding the answer.
func ObserveAssistantInputTx(ctx context.Context, tx *sql.Tx, inputID int64) (*AssistantObservation, error) {
	in, err := loadInboxInput(ctx, tx, inputID)
	if err != nil {
		return nil, err
	}

	record, err := scanSession(
		tx.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, in.SessionID),
	)
	if err != nil {
		return nil, err
	}

	view := &AssistantObservation{Input: in, Session: record}
	if in.State != InputStateAccepted {
		return view, nil
	}

	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(accepted_message_id), 0) FROM session_inbox
		WHERE session_id = ? AND source = 'user' AND state = 'accepted' AND accepted_message_id > ?`,
		in.SessionID, in.AcceptedMessageID).Scan(&view.NextUser); err != nil {
		return nil, fmt.Errorf("bound assistant answer: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `SELECT id, role, COALESCE(content,''), COALESCE(finish_type,''),
		COALESCE(tool_calls, '[]') FROM messages WHERE session_id = ? AND id > ? AND (? = 0 OR id <= ?)
		AND rejected_reason IS NULL AND compacted_at IS NULL ORDER BY position IS NULL, position, id`,
		in.SessionID, in.AcceptedMessageID, view.NextUser, view.NextUser)
	if err != nil {
		return nil, fmt.Errorf("load assistant answer window: %w", err)
	}
	defer rows.Close()

	messages, err := scanAssistantWindow(rows)
	if err != nil {
		return nil, err
	}

	view.Answer = DeriveFinalAnswer(messages, in.AcceptedMessageID, view.NextUser)
	view.Settled = len(messages) > 0 && IsFinalAssistantMessage(messages[len(messages)-1]) &&
		record.CompletionCheckCandidateID == nil

	return view, nil
}

// DeriveFinalAnswer joins every final in the window, oldest first: a closing final often
// only points back at an earlier one that holds the substance.
func DeriveFinalAnswer(messages []*transcript.Message, afterID, upperID int64) string {
	var finals []string

	for _, message := range messages {
		if message.ID <= afterID || (upperID > 0 && message.ID > upperID) || !IsFinalAssistantMessage(message) {
			continue
		}

		if text := strings.TrimSpace(message.Content); !slices.Contains(finals, text) {
			finals = append(finals, text)
		}
	}

	return strings.Join(finals, "\n\n---\n\n")
}

// IsFinalAssistantMessage excludes tool, empty, truncated and rejected responses.
func IsFinalAssistantMessage(message *transcript.Message) bool {
	return message.Role == "assistant" && message.RejectedReason == "" && len(message.ToolCalls) == 0 &&
		strings.TrimSpace(message.Content) != "" && (message.FinishType == "" || message.FinishType == "stop")
}

func scanAssistantWindow(rows *sql.Rows) ([]*transcript.Message, error) {
	var messages []*transcript.Message

	for rows.Next() {
		message := &transcript.Message{}
		var calls string

		if err := rows.Scan(&message.ID, &message.Role, &message.Content, &message.FinishType, &calls); err != nil {
			return nil, fmt.Errorf("scan assistant answer: %w", err)
		}

		if calls != "[]" && calls != "null" && calls != "" {
			message.ToolCalls = []byte(calls)
		}

		messages = append(messages, message)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read assistant answer: %w", err)
	}

	return messages, nil
}
