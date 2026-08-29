package sessionstore

import (
	"context"
	"errors"
	"fmt"
)

// ErrProgressSuperseded reports that a progress snapshot no longer describes the
// session: its generation or status moved on (or the root stopped) before the
// card committed. Nothing is inserted and no observer message may be published.
var ErrProgressSuperseded = errors.New("progress snapshot superseded")

// EnqueueProgressOutput inserts one replaceable progress card only while the
// session still sits at the snapshot's generation and status, so a stale card
// is discarded instead of publishing below a newer transition.
func (s *store) EnqueueProgressOutput(
	ctx context.Context,
	draft OutputDraft,
	expectedGeneration int64,
	expectedStatus SessionStatus,
) (*OutputCommit, error) {
	if err := validateOutputDraft(draft); err != nil {
		return nil, err
	}

	if !expectedStatus.valid() {
		return nil, fmt.Errorf("invalid expected progress status %q", expectedStatus)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin progress output: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		generation int64
		status     SessionStatus
	)
	if err := tx.QueryRowContext(ctx,
		`SELECT model_input_generation, status FROM sessions WHERE id = ?`, draft.SessionID,
	).Scan(&generation, &status); err != nil {
		return nil, fmt.Errorf("load progress eligibility: %w", err)
	}

	if generation != expectedGeneration || status != expectedStatus ||
		status == SessionStatusStopping || status == SessionStatusStopped {
		return nil, ErrProgressSuperseded
	}

	commit, err := enqueueOutputTx(ctx, tx, draft)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit progress output: %w", err)
	}

	return commit, nil
}
