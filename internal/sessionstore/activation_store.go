package sessionstore

import (
	"context"
	"fmt"
)

// TryFinalizeSubagentActivation linearizes terminalization against durable
// follow-up acceptance and /stop. All participating tables are owned by this
// store, so SQLite is the synchronization primitive rather than a Go mutex held
// across persistence calls.
func (s *store) TryFinalizeSubagentActivation(
	ctx context.Context,
	childID int64,
	state, result, outcome string,
) (bool, error) {
	if state != "completed" && state != "error" {
		return false, fmt.Errorf("invalid activation terminal state %q", state)
	}

	if outcome != "completed" && outcome != "error" && outcome != "incomplete" {
		return false, fmt.Errorf("invalid activation outcome %q", outcome)
	}

	execResult, err := s.db.ExecContext(ctx, `
		UPDATE subagent_links
		SET state = ?, result = ?, outcome = ?
		WHERE child_id = ?
		  AND state IN ('spawned', 'running')
		  AND EXISTS (
			SELECT 1 FROM sessions sess
			WHERE sess.id = subagent_links.child_id
			  AND sess.status NOT IN ('stopping', 'stopped', 'killed')
			  AND sess.killed_at IS NULL
		  )
		  AND NOT EXISTS (
			SELECT 1 FROM session_inbox input
			WHERE input.session_id = subagent_links.child_id
			  AND input.state = 'pending'
		  )`, state, result, outcome, childID)
	if err != nil {
		return false, fmt.Errorf("conditionally finalize subagent activation: %w", err)
	}

	rows, err := execResult.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("finalize subagent activation rows affected: %w", err)
	}

	return rows == 1, nil
}

// RearmDeliveredSubagentWithPendingInput begins the next serialized activation
// only after the previous outcome is durably present in the parent transcript.
func (s *store) RearmDeliveredSubagentWithPendingInput(ctx context.Context, childID int64) (bool, error) {
	execResult, err := s.db.ExecContext(ctx, `
		UPDATE subagent_links
		SET state = 'running', blocking = 0,
		    activation_seq = activation_seq + 1,
		    delivered_at = NULL, delivered_msg_id = NULL
		WHERE child_id = ?
		  AND state IN ('completed', 'error')
		  AND delivered_at IS NOT NULL
		  AND EXISTS (
			SELECT 1 FROM session_inbox input
			WHERE input.session_id = subagent_links.child_id
			  AND input.state = 'pending'
		  )`, childID)
	if err != nil {
		return false, fmt.Errorf("rearm delivered subagent activation: %w", err)
	}

	rows, err := execResult.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rearm subagent activation rows affected: %w", err)
	}

	return rows == 1, nil
}
