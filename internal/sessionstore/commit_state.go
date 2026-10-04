package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

func checkCommitFence(ctx context.Context, tx *sql.Tx, c Commit) error {
	var status SessionStatus

	var killed sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT status,killed_at FROM sessions WHERE id = ?`, c.SessionID).
		Scan(&status, &killed); err != nil {
		return fmt.Errorf("load session commit fence: %w", err)
	}

	if killed.Valid || status == SessionStatusKilled || status == SessionStatusTerminating ||
		(status == SessionStatusStopping && c.Mode != CommitLifecycle) {
		return ErrSessionStopping
	}

	return nil
}

func commitAcceptTx(
	ctx context.Context,
	tx *sql.Tx,
	c Commit,
	accept Accept,
	linkedID int64,
	result *CommitResult,
) error {
	input, err := loadInboxInput(ctx, tx, accept.InputID)
	if err != nil {
		return err
	}

	if input.SessionID != c.SessionID {
		return ErrInputNotFound
	}

	if input.State == InputStateAccepted && accept.State == InputStateAccepted {
		if linkedID == 0 {
			result.MessageIDs = append(result.MessageIDs, input.AcceptedMessageID)
		}

		return nil
	}

	if input.State != InputStatePending {
		return ErrInputResolved
	}

	if accept.State != InputStateAccepted {
		return resolveCommitInputTx(ctx, tx, c, accept, input)
	}

	id := linkedID
	if id == 0 {
		message, err := insertPromotedMessage(ctx, tx, input, accept.Content)
		if err != nil {
			return err
		}

		id = message.ID
		result.MessageIDs = append(result.MessageIDs, id)
	}

	if err := acceptPendingInput(ctx, tx, input.ID, id, c.At); err != nil {
		return err
	}

	if err := activatePromotedInputSession(ctx, tx, c.SessionID, input.ID, c.At); err != nil {
		return err
	}

	if accept.ModelBound {
		if err := acceptModelInputTx(ctx, tx, c.SessionID, id, input); err != nil {
			return err
		}
	}

	if accept.InvalidateCompletion && !accept.ModelBound {
		if err := invalidateCompletionCheckTx(ctx, tx, c.SessionID); err != nil {
			return err
		}
	}

	if accept.Receipt != "" {
		return commitAcceptReceiptTx(ctx, tx, c, input, accept.Receipt, result)
	}

	return nil
}

func commitActivationTx(ctx context.Context, tx *sql.Tx, c Commit) (*ToolActivation, error) {
	change := c.Activation
	switch change.State {
	case "", ActivationPending:
		input, err := loadInboxInput(ctx, tx, change.InputID)
		if err != nil {
			return nil, err
		}

		if input.SessionID != c.SessionID || input.State != InputStateAccepted {
			return nil, ErrActivationConflict
		}

		existing, err := scanActivation(
			tx.QueryRowContext(
				ctx,
				`SELECT input_id,session_id,tool_id,command,state,COALESCE(tool_call_id,''),created_at,resolved_at
   FROM session_tool_activations WHERE input_id = ?`,
				change.InputID,
			),
		)
		if err == nil {
			if existing.ToolID != change.ToolID || existing.Command != change.Command {
				return nil, ErrActivationConflict
			}

			return existing, nil
		}

		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("load activation: %w", err)
		}

		return insertActivation(ctx, tx, input, ActivationDraft{ToolID: change.ToolID, Command: change.Command}, c.At)
	case ActivationConsumed:
		if err := ConsumeActivationTx(ctx, tx, ActivationBinding{
			InputID: change.InputID, SessionID: c.SessionID,
			ToolID: change.ToolID, Command: change.Command, ToolCallID: change.ToolCallID,
		}); err != nil {
			return nil, err
		}
	case ActivationExpired:
		if _, err := tx.ExecContext(ctx, `UPDATE session_tool_activations SET state = 'expired',resolved_at = ?
   WHERE input_id = ? AND session_id = ? AND state = 'pending'`, c.At, change.InputID, c.SessionID); err != nil {
			return nil, fmt.Errorf("expire activation: %w", err)
		}
	default:
		return nil, ErrActivationConflict
	}

	grant, err := scanActivation(
		tx.QueryRowContext(
			ctx,
			`SELECT input_id,session_id,tool_id,command,state,COALESCE(tool_call_id,''),created_at,resolved_at
  FROM session_tool_activations WHERE input_id = ? AND session_id = ?`,
			change.InputID,
			c.SessionID,
		),
	)
	if err != nil {
		return nil, fmt.Errorf("load changed activation: %w", err)
	}

	return grant, nil
}

func applyStatePatchTx(
	ctx context.Context,
	tx *sql.Tx,
	sessionID int64,
	state StatePatch,
	ids []int64,
	at time.Time,
) error {
	if state.Candidate != nil {
		if err := applyCandidatePatchTx(ctx, tx, sessionID, state.Candidate, ids, at); err != nil {
			return err
		}
	}
	var sets []string
	var args []any

	if state.Iteration != nil {
		sets = append(sets, "iteration = ?")
		args = append(args, *state.Iteration)
	}

	if state.Status != nil {
		if !state.Status.valid() {
			return errors.New("invalid session status")
		}

		sets = append(sets, "status = ?")
		args = append(args, *state.Status)
	}

	if state.TodoItems != nil {
		sets = append(sets, "todo_items = ?")
		args = append(args, string(*state.TodoItems))
	}

	sets, args = appendContextStateFields(sets, args, state)

	if state.ConfirmedAnswerID != nil {
		sets = append(sets, "completion_check_confirmed_answer_id = ?")
		args = append(args, nullMessageID(*state.ConfirmedAnswerID))
	}

	if state.EmptyStopStreak != nil {
		sets = append(sets, "empty_stop_streak = ?")
		args = append(args, *state.EmptyStopStreak)
	}

	if state.ManagerReplyPending != nil {
		sets = append(sets, "manager_reply_pending = ?")
		args = append(args, *state.ManagerReplyPending)
	}

	if len(sets) == 0 {
		return nil
	}

	if state.Iteration != nil || state.Status != nil {
		sets = append(sets, "updated_at = ?")
		args = append(args, at)
	}

	args = append(args, sessionID)
	//nolint:gosec // StatePatch selects fixed column names; every value is bound.
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE sessions SET `+strings.Join(sets, ",")+` WHERE id = ?`,
		args...); err != nil {
		return fmt.Errorf("apply session state: %w", err)
	}

	return nil
}

func resetContextTx(ctx context.Context, tx *sql.Tx, sessionID int64, at time.Time) error {
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE messages SET compacted_at = ? WHERE session_id = ? AND compacted_at IS NULL`,
		at,
		sessionID,
	); err != nil {
		return fmt.Errorf("reset transcript: %w", err)
	}

	if _, err := tx.ExecContext(
		ctx,
		`UPDATE sessions SET todo_items = '[]',context_baseline_model = '',context_baseline_prompt_tokens = 0,
  context_baseline_message_count = 0,completion_check_candidate_id = NULL,completion_check_confirmed_answer_id = NULL,empty_stop_streak = 0 WHERE id = ?`,
		sessionID,
	); err != nil {
		return fmt.Errorf("reset context state: %w", err)
	}

	return nil
}

func resolveCommitInputTx(ctx context.Context, tx *sql.Tx, c Commit, accept Accept, input *InboxInput) error {
	if accept.State != InputStateHandled && accept.State != InputStateRejected &&
		accept.State != InputStateCancelled ||
		accept.Reason == "" {
		return errors.New("invalid input resolution")
	}

	_, err := tx.ExecContext(
		ctx,
		`UPDATE session_inbox SET state = ?,resolved_at = ?,resolution_reason = ? WHERE id = ? AND state = 'pending'`,
		accept.State,
		c.At,
		accept.Reason,
		input.ID,
	)
	if err == nil && accept.InvalidateCompletion {
		return invalidateCompletionCheckTx(ctx, tx, c.SessionID)
	}

	if err != nil {
		return fmt.Errorf("commit accept tx: %w", err)
	}

	return nil
}

func acceptModelInputTx(ctx context.Context, tx *sql.Tx, sessionID, id int64, input *InboxInput) error {
	if err := advanceModelInputGeneration(ctx, tx, sessionID, id); err != nil {
		return err
	}

	if err := invalidateCompletionCheckTx(ctx, tx, sessionID); err != nil {
		return err
	}

	if owner, _ := input.Attributes[managerIDAttribute].(string); owner != "" &&
		!readOnlyCommandReceipt(input.RawContent) {
		if err := setManagerReplyPendingTx(ctx, tx, sessionID); err != nil {
			return err
		}
	}

	return nil
}

func applyCandidatePatchTx(
	ctx context.Context,
	tx *sql.Tx,
	sessionID int64,
	change *CandidateChange,
	ids []int64,
	at time.Time,
) error {
	next := change.Next
	if change.NextRef >= 0 {
		if change.NextRef >= len(ids) {
			return errors.New("candidate message reference out of range")
		}

		next = ids[change.NextRef]
	}

	return setCompletionCandidate(ctx, tx, sessionID, change.Expected, next, at)
}

func appendContextStateFields(sets []string, args []any, state StatePatch) ([]string, []any) {
	if state.ClearContextBaseline {
		sets = append(
			sets,
			"context_baseline_model = ''",
			"context_baseline_prompt_tokens = 0",
			"context_baseline_message_count = 0",
		)
	}

	if state.ContextBaseline != nil {
		sets = append(
			sets,
			"context_baseline_model = ?",
			"context_baseline_prompt_tokens = ?",
			"context_baseline_message_count = ?",
		)
		args = append(
			args,
			state.ContextBaseline.Model,
			state.ContextBaseline.PromptTokens,
			state.ContextBaseline.MessageCount,
		)
	}

	return sets, args
}

func commitAcceptReceiptTx(
	ctx context.Context,
	tx *sql.Tx,
	c Commit,
	input *InboxInput,
	receipt string,
	result *CommitResult,
) error {
	output, err := insertOutputTx(ctx, tx, OutputDraft{
		SessionID:     c.SessionID,
		Type:          OutputMessagePersistent,
		Content:       receipt,
		SourceKey:     fmt.Sprintf("input:%d:skill_receipt", input.ID),
		ReleasesInput: true,
		CreatedAt:     c.At,
	}, c.Mode)
	if err != nil {
		return err
	}

	if output != nil {
		result.Outputs = append(result.Outputs, output)
	}

	return nil
}
