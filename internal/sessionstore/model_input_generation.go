package sessionstore

import (
	"context"
	"fmt"
)

// Only model-bound input advances the generation, in the same transaction
// that inserts its transcript row.
func advanceModelInputGeneration(ctx context.Context, q execer, sessionID int64) error {
	result, err := q.ExecContext(ctx, `
		UPDATE sessions
		SET model_input_generation = model_input_generation + 1
		WHERE id = ?`, sessionID)
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
