package sessionstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// advanceModelInputGeneration commits one model-input boundary inside the
// caller's transaction: the generation increments and the boundary moves to the
// transcript row that just entered history. Callers must only invoke it in the
// same transaction that inserted that row, and only for genuine model-bound
// input — pending inbox insertion, compaction, tool results, external-call
// completions, and host-handled commands must not advance the generation.
func advanceModelInputGeneration(ctx context.Context, q execer, sessionID, boundary int64) error {
	result, err := q.ExecContext(ctx, `
		UPDATE sessions
		SET model_input_generation = model_input_generation + 1, model_input_boundary = ?
		WHERE id = ?`, boundary, sessionID)
	if err != nil {
		return fmt.Errorf("advance model input generation for session %d: %w", sessionID, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("advance generation rows affected: %w", err)
	}

	if rows != 1 {
		return fmt.Errorf("session %d not found during generation advance", sessionID)
	}

	return nil
}

// InvalidateCompletionCheckTx clears a stale completion check and its empty
// streak inside the caller's transaction, in the same commit as the external
// model-visible input that supersedes them. The typed host completion nudge is
// the only ingress that must not call this. Idempotent replays that insert no
// new model input never reach it. Exported for cross-package transactions
// (subagent delivery) whose tests may not import this package back.
func InvalidateCompletionCheckTx(ctx context.Context, tx *sql.Tx, sessionID int64, _ time.Time) error {
	return invalidateCompletionCheckTx(ctx, tx, sessionID)
}

// invalidateCompletionCheckTx clears a stale completion check and its empty
// streak inside the caller's transaction, in the same commit as the external
// model-visible input that supersedes them.
func invalidateCompletionCheckTx(ctx context.Context, q execer, sessionID int64) error {
	result, err := q.ExecContext(ctx, `
		UPDATE sessions
		SET completion_check_candidate_id = NULL, empty_stop_streak = 0,
			completion_check_confirmed_answer_id = NULL
		WHERE id = ? AND (completion_check_candidate_id IS NOT NULL OR empty_stop_streak <> 0
			OR completion_check_confirmed_answer_id IS NOT NULL)`,
		sessionID)
	if err != nil {
		return fmt.Errorf("invalidate completion check for session %d: %w", sessionID, err)
	}

	if _, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("invalidate completion check rows affected: %w", err)
	}

	return nil
}

// setManagerReplyPendingTx records the durable manager-reply obligation in the
// same transaction that promotes a manager-owned model input.
func setManagerReplyPendingTx(ctx context.Context, q execer, sessionID int64) error {
	result, err := q.ExecContext(ctx, `
		UPDATE sessions SET manager_reply_pending = TRUE
		WHERE id = ? AND manager_reply_pending = FALSE`, sessionID)
	if err != nil {
		return fmt.Errorf("set manager reply pending for session %d: %w", sessionID, err)
	}

	if _, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("set manager reply pending rows affected: %w", err)
	}

	return nil
}

// clearManagerReplyPendingTx settles the durable manager-reply obligation in
// the same transaction that commits a terminal lifecycle output superseding
// the owed reply.
func clearManagerReplyPendingTx(ctx context.Context, q execer, sessionID int64, now time.Time) error {
	result, err := q.ExecContext(ctx, `
		UPDATE sessions SET manager_reply_pending = FALSE, updated_at = ?
		WHERE id = ? AND manager_reply_pending = TRUE`, now, sessionID)
	if err != nil {
		return fmt.Errorf("clear manager reply pending for session %d: %w", sessionID, err)
	}

	if _, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("clear manager reply pending rows affected: %w", err)
	}

	return nil
}
