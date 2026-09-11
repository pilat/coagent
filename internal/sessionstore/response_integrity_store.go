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
	OutputLengthRecoveryPrompt  = "[AUTOMATED RECOVERY: The previous model response reached its output limit and was discarded. Repeat the current step as a complete, concise response. If tools are needed, split the work into smaller calls.]"
	OutputLengthTerminalError   = "Model response reached the output limit twice."
	UnknownFinishTerminalError  = "Model response ended with an unknown finish reason."
	RejectedReasonOutputLength  = "output_length"
	RejectedReasonUnknownFinish = "unknown_finish"
)

const integrityErrorPrefix = "❌ LLM error: "

const (
	RejectedResponseRecoveryQueued   RejectedResponseOutcome = "recovery_queued"
	RejectedResponseRetryExhausted   RejectedResponseOutcome = "retry_exhausted"
	RejectedResponseUnknownTerminal  RejectedResponseOutcome = "unknown_terminal"
	RejectedResponseBudgetSuppressed RejectedResponseOutcome = "budget_suppressed"
)

var (
	_ ResponseIntegrityStore = (*store)(nil)
	_ TerminalRejectionStore = (*store)(nil)
)

type RejectedResponseOutcome string

type RejectedResponse struct {
	SessionID  int64
	RootID     int64
	Iteration  int
	Message    *transcript.Message
	ObservedAt time.Time
}

type RejectedResponseResult struct {
	MessageID         int64
	RecoveryMessageID int64
	Outcome           RejectedResponseOutcome
	Budget            *BudgetRecord
	Output            *OutputCommit
}

type ResponseIntegrityStore interface {
	CommitRejectedResponse(
		ctx context.Context,
		rejection RejectedResponse,
	) (*RejectedResponseResult, error)
	HasOutstandingResponseRecovery(ctx context.Context, sessionID int64) (bool, error)
}

type TerminalRejectionStore interface {
	LoadCurrentTerminalRejection(ctx context.Context, sessionID int64) (*transcript.Message, error)
}

func IntegrityErrorNotice(errorText string) string {
	return integrityErrorPrefix + errorText
}

// CommitRejectedResponse is the crash boundary for an unusable model attempt:
// attempt, iteration, budget decision, and sole disposition commit together.
func (s *store) CommitRejectedResponse(
	ctx context.Context,
	rejection RejectedResponse,
) (*RejectedResponseResult, error) {
	if err := validateRejectedResponse(rejection); err != nil {
		return nil, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin rejected response: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := rejection.ObservedAt.UTC()
	if rejection.ObservedAt.IsZero() {
		now = time.Now().UTC()
	}

	messageID, err := insertMessageWith(ctx, tx, rejection.SessionID, rejection.Message)
	if err != nil {
		return nil, err
	}

	result, err := commitRejectedOutcome(ctx, tx, rejection, messageID, now)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit rejected response: %w", err)
	}

	return result, nil
}

func (s *store) LoadCurrentTerminalRejection(
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

func (s *store) HasOutstandingResponseRecovery(ctx context.Context, sessionID int64) (bool, error) {
	return hasOutstandingRecovery(ctx, s.db, sessionID)
}

func commitRejectedOutcome(
	ctx context.Context,
	tx *sql.Tx,
	rejection RejectedResponse,
	messageID int64,
	now time.Time,
) (*RejectedResponseResult, error) {
	result := &RejectedResponseResult{MessageID: messageID}

	record, output, suppressed, err := commitRejectedBudget(ctx, tx, rejection, now)
	if err != nil {
		return nil, err
	}

	result.Budget, result.Output = record, output

	switch {
	case suppressed:
		result.Outcome = RejectedResponseBudgetSuppressed
		err = updateRejectedIteration(ctx, tx, rejection.SessionID, rejection.Iteration, false, now)
	case rejection.Message.RejectedReason == RejectedReasonUnknownFinish:
		result.Outcome = RejectedResponseUnknownTerminal
		result.Output, err = commitRejectedTerminal(
			ctx, tx, rejection.SessionID, rejection.Iteration, UnknownFinishTerminalError, now,
		)
	default:
		err = commitRejectedLength(ctx, tx, rejection, result, messageID, now)
	}

	if err != nil {
		return nil, err
	}

	return result, nil
}

func commitRejectedLength(
	ctx context.Context,
	tx *sql.Tx,
	rejection RejectedResponse,
	result *RejectedResponseResult,
	messageID int64,
	now time.Time,
) error {
	outstanding, err := hasOutstandingRecovery(ctx, tx, rejection.SessionID)
	if err != nil {
		return err
	}

	if outstanding {
		result.Outcome = RejectedResponseRetryExhausted
		result.Output, err = commitRejectedTerminal(
			ctx, tx, rejection.SessionID, rejection.Iteration, OutputLengthTerminalError, now,
		)

		return err
	}

	result.Outcome = RejectedResponseRecoveryQueued

	result.RecoveryMessageID, err = insertMessageWith(ctx, tx, rejection.SessionID, &transcript.Message{
		Role: "user", Content: OutputLengthRecoveryPrompt, RetryOfMessageID: messageID,
	})
	if err != nil {
		return err
	}

	return updateRejectedIteration(ctx, tx, rejection.SessionID, rejection.Iteration, false, now)
}

func validateRejectedResponse(rejection RejectedResponse) error {
	if rejection.SessionID <= 0 || rejection.RootID <= 0 || rejection.Iteration < 0 ||
		rejection.Message == nil || rejection.Message.Role != assistantRole {
		return errors.New("invalid rejected response")
	}

	switch {
	case rejection.Message.FinishType == "length" &&
		rejection.Message.RejectedReason == RejectedReasonOutputLength:
		return nil
	case rejection.Message.FinishType == "unknown" &&
		rejection.Message.RejectedReason == RejectedReasonUnknownFinish:
		return nil
	default:
		return errors.New("invalid rejected response disposition")
	}
}

//nolint:wsl_v5 // Query selection and execution form one small update.
func updateRejectedIteration(
	ctx context.Context,
	tx *sql.Tx,
	sessionID int64,
	iteration int,
	terminal bool,
	now time.Time,
) error {
	query := `UPDATE sessions SET iteration = ?, updated_at = ?
		WHERE id = ? AND killed_at IS NULL AND status NOT IN ('stopping', 'terminating', 'killed')`
	args := []any{iteration, now, sessionID}
	if terminal {
		query = `UPDATE sessions SET iteration = ?, status = 'error', updated_at = ?
			WHERE id = ? AND killed_at IS NULL AND status NOT IN ('stopping', 'terminating', 'killed')`
	}

	result, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		return fmt.Errorf("update rejected response iteration: %w", err)
	}

	return requireOneSessionUpdate(result, sessionID)
}

//nolint:wsl_v5 // The durable-state query stays adjacent to its scan.
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

//nolint:wsl_v5 // Terminal state and its optional output are one transaction fragment.
func commitRejectedTerminal(
	ctx context.Context,
	tx *sql.Tx,
	sessionID int64,
	iteration int,
	errorText string,
	now time.Time,
) (*OutputCommit, error) {
	if err := updateRejectedIteration(ctx, tx, sessionID, iteration, true, now); err != nil {
		return nil, err
	}

	owner, err := outputOwner(ctx, tx, sessionID)
	if errors.Is(err, ErrOutputOwner) || errors.Is(err, ErrOutputNotRoot) {
		return &OutputCommit{}, nil
	}
	if err != nil {
		return nil, err
	}

	return insertMessageOutput(
		ctx, tx, sessionID, owner, IntegrityErrorNotice(errorText),
		fmt.Sprintf("integrity:%d:error", iteration), now, true,
	)
}
