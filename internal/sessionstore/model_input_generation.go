package sessionstore

import (
	"context"
	"fmt"
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
