package sessionstore

import (
	"context"
	"fmt"

	"github.com/pilat/coagent/internal/transcript"
)

const (
	OutputLengthRecoveryPrompt  = "[AUTOMATED RECOVERY: The previous model response reached its output limit and was discarded. Repeat the current step as a complete, concise response. If tools are needed, split the work into smaller calls.]"
	OutputLengthTerminalError   = "Model response reached the output limit twice."
	UnknownFinishTerminalError  = "Model response ended with an unknown finish reason."
	RejectedReasonOutputLength  = "output_length"
	RejectedReasonUnknownFinish = "unknown_finish"
)

const integrityErrorPrefix = "❌ LLM error: "

func IntegrityErrorNotice(errorText string) string {
	return integrityErrorPrefix + errorText
}

func (s *Store) LoadCurrentTerminalRejection(
	ctx context.Context,
	sessionID int64,
) (*transcript.Message, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id, session_id, role, content, tool_call_id, tool_name,
		tool_error, tool_calls, reasoning_content, reasoning_raw, attachments, cost_usd, usage,
		finish_type, provider_finish_reason, rejected_reason, retry_of_message_id, compacted_at, created_at
		FROM messages rejected WHERE session_id = ? AND role = 'assistant' AND rejected_reason IS NOT NULL
			AND EXISTS (SELECT 1 FROM sessions WHERE sessions.id = rejected.session_id
				AND sessions.status = 'error')
			AND id > COALESCE((SELECT MAX(input.accepted_message_id) FROM session_inbox input
				WHERE input.session_id = ? AND input.state = 'accepted'
					AND input.accepted_message_id IS NOT NULL), 0)
			AND (rejected_reason = 'unknown_finish' OR EXISTS (
				SELECT 1 FROM messages recovery
				WHERE recovery.session_id = rejected.session_id
					AND recovery.retry_of_message_id IS NOT NULL AND recovery.id < rejected.id
					AND recovery.id > COALESCE((SELECT MAX(input.accepted_message_id)
						FROM session_inbox input WHERE input.session_id = rejected.session_id
							AND input.state = 'accepted' AND input.accepted_message_id IS NOT NULL), 0)
			))
		ORDER BY id DESC LIMIT 1`, sessionID, sessionID)

	message, err := scanMessage(row)
	if err != nil {
		return nil, fmt.Errorf("load current terminal rejection: %w", err)
	}

	return message, nil
}

func (s *Store) HasOutstandingResponseRecovery(ctx context.Context, sessionID int64) (bool, error) {
	return hasOutstandingRecovery(ctx, s.db, sessionID)
}

func hasOutstandingRecovery(ctx context.Context, q queryer, sessionID int64) (bool, error) {
	var outstanding bool

	err := q.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM messages recovery
		WHERE recovery.session_id = ? AND recovery.retry_of_message_id IS NOT NULL
			AND recovery.id > COALESCE((SELECT MAX(input.accepted_message_id) FROM session_inbox input
				WHERE input.session_id = ? AND input.state = 'accepted'
					AND input.accepted_message_id IS NOT NULL), 0)
	)`, sessionID, sessionID).Scan(&outstanding)
	if err != nil {
		return false, fmt.Errorf("load outstanding response recovery: %w", err)
	}

	return outstanding, nil
}
