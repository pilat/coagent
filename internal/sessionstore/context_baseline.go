package sessionstore

import (
	"context"
	"fmt"
)

// ContextBaseline is the last provider-measured context size, persisted across
// restarts. A measurement belongs to one model's window and tokenizer, so the
// model travels with the numbers.
type ContextBaseline struct {
	Model        string
	PromptTokens int
	MessageCount int
}

// ContextBaseline returns the persisted measurement, or nil when none was
// taken on any model.
func (r *SessionRecord) ContextBaseline() *ContextBaseline {
	if r.ContextBaselineModel == "" || r.ContextBaselinePromptTokens <= 0 {
		return nil
	}

	return &ContextBaseline{
		Model:        r.ContextBaselineModel,
		PromptTokens: r.ContextBaselinePromptTokens,
		MessageCount: r.ContextBaselineMessageCount,
	}
}

// SaveContextBaseline persists the measurement. It deliberately leaves
// updated_at alone: derived bookkeeping must not float a session to the top of
// activity-ordered listings.
func (s *store) SaveContextBaseline(ctx context.Context, sessionID int64, b ContextBaseline) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET context_baseline_model = ?, context_baseline_prompt_tokens = ?, context_baseline_message_count = ?
		WHERE id = ?`,
		b.Model, b.PromptTokens, b.MessageCount, sessionID,
	)
	if err != nil {
		return fmt.Errorf("save context baseline: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("save context baseline: %w", err)
	}

	if rows == 0 {
		return fmt.Errorf("session %d not found", sessionID)
	}

	return nil
}

// ClearContextBaseline drops the persisted measurement — the transcript it
// described is gone (compaction commit, context reset).
func (s *store) ClearContextBaseline(ctx context.Context, sessionID int64) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET context_baseline_model = '', context_baseline_prompt_tokens = 0, context_baseline_message_count = 0
		WHERE id = ?`,
		sessionID,
	)
	if err != nil {
		return fmt.Errorf("clear context baseline: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("clear context baseline: %w", err)
	}

	if rows == 0 {
		return fmt.Errorf("session %d not found", sessionID)
	}

	return nil
}
