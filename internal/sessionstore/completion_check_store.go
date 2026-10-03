package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const EmptyStopTerminalStreak = 6

var (
	// ErrCompletionCheckConflict reports a stale or mismatched candidate
	// identity: the caller's check no longer owns the durable transition.
	ErrCompletionCheckConflict = errors.New("completion check candidate conflict")
)

type CompletionCheckState struct {
	CandidateID *int64
	// CandidateText resolves CandidateID's messages.content in the same read,
	// so a confirming disposition can publish the candidate instead of the
	// nudge ack. Empty when no candidate is pending.
	CandidateText       string
	ManagerReplyPending bool
	EmptyStopStreak     int
}

// EmptyStopTerminalNotice is the durable host notice committed for the sixth
// consecutive empty response. Sessionlifecycle reuses it as a child's
// recovered result instead of fabricating a model-authored answer.
func EmptyStopTerminalNotice(count int) string {
	return fmt.Sprintf(
		"⚠️ Model returned %d consecutive empty responses. Session paused — waiting for input.",
		count,
	)
}

// updateDispositionIteration advances the iteration, stamps the empty streak,
// and preserves the manager reply obligation carried by the caller.
func (s *Store) LoadCompletionCheckState(ctx context.Context, sessionID int64) (*CompletionCheckState, error) {
	var candidate sql.NullInt64
	var candidateText sql.NullString
	var replyPending sql.NullBool
	var streak sql.NullInt64

	err := s.db.QueryRowContext(ctx, `SELECT sessions.completion_check_candidate_id,
		messages.content, sessions.manager_reply_pending, sessions.empty_stop_streak
		FROM sessions LEFT JOIN messages ON messages.id = sessions.completion_check_candidate_id
		WHERE sessions.id = ?`, sessionID).
		Scan(&candidate, &candidateText, &replyPending, &streak)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errSessionNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("load completion check state: %w", err)
	}

	state := &CompletionCheckState{
		CandidateText:       candidateText.String,
		ManagerReplyPending: replyPending.Bool,
		EmptyStopStreak:     int(streak.Int64),
	}
	if candidate.Valid {
		state.CandidateID = &candidate.Int64
	}

	return state, nil
}

func setCompletionCandidate(
	ctx context.Context,
	tx *sql.Tx,
	sessionID, expected, next int64,
	now time.Time,
) error {
	var result sql.Result

	var err error

	// next == 0 means "clear the check", which the column encodes as NULL.
	nextValue := any(next)
	if next == 0 {
		nextValue = nil
	}

	where := "completion_check_candidate_id IS NULL"
	args := []any{nextValue, now, sessionID}

	if expected != 0 {
		where = "completion_check_candidate_id = ?"

		args = append(args, expected)
	}

	result, err = tx.ExecContext(ctx, `UPDATE sessions
		SET completion_check_candidate_id = ?, updated_at = ?
		WHERE id = ? AND `+where, args...)
	if err != nil {
		return fmt.Errorf("update completion check candidate: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("count completion check update: %w", err)
	}

	if affected == 0 {
		var current sql.NullInt64

		if scanErr := tx.QueryRowContext(ctx, `SELECT completion_check_candidate_id
			FROM sessions WHERE id = ?`, sessionID).Scan(&current); scanErr != nil {
			return fmt.Errorf("load completion check state: %w", scanErr)
		}

		return fmt.Errorf("%w: session %d (expected %d, current %d valid %t)",
			ErrCompletionCheckConflict, sessionID, expected, current.Int64, current.Valid)
	}

	return requireOneSessionUpdate(result, sessionID)
}
