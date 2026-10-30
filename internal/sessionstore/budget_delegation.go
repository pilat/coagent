package sessionstore

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/pilat/coagent/internal/budget"
)

func delegatedBudgetMutation(mutation budget.Mutation) bool {
	return mutation.InputID == 0 && mutation.Command == ""
}

func validateBudgetBinding(mutation budget.Mutation) error {
	if delegatedBudgetMutation(mutation) {
		if mutation.GrantSessionID <= 0 || mutation.RootSessionID == mutation.GrantSessionID {
			return budget.ErrConflict
		}
	} else if mutation.InputID <= 0 || mutation.Command == "" {
		return budget.ErrConflict
	}

	return nil
}

func budgetMutationFacts(ctx context.Context, tx *sql.Tx, mutation budget.Mutation) (float64, error) {
	if err := authorizeDelegatedBudgetTx(ctx, tx, mutation); err != nil {
		return 0, err
	}

	return budgetRootFacts(ctx, tx, mutation.RootSessionID)
}

func authorizeDelegatedBudgetTx(ctx context.Context, tx *sql.Tx, mutation budget.Mutation) error {
	if !delegatedBudgetMutation(mutation) {
		return nil
	}

	var allowed bool

	err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM sessions receipt JOIN sessions target ON target.id = ?
		WHERE receipt.id = ? AND receipt.agent_type = 'assistant' AND receipt.parent_id = 0
		AND target.parent_id = 0 AND COALESCE(target.agent_type, '') <> 'assistant'
		AND target.killed_at IS NULL AND target.status NOT IN ('terminating', 'killed')
		AND COALESCE(json_extract(receipt.attributes, '$.manager_id'), '') <> ''
		AND json_extract(target.attributes, '$.manager_id') = json_extract(receipt.attributes, '$.manager_id')
	)`, mutation.RootSessionID, mutation.GrantSessionID).Scan(&allowed)
	if err != nil {
		return fmt.Errorf("authorize delegated budget: %w", err)
	}

	if !allowed {
		return budget.ErrConflict
	}

	return nil
}

func replayDelegatedBudgetTx(ctx context.Context, tx *sql.Tx, mutation budget.Mutation) (*budget.Record, error) {
	var exists bool

	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_outbox
		WHERE session_id = ? AND source_key = ?)`, mutation.GrantSessionID,
		fmt.Sprintf("tool:%s:direct:0", mutation.ToolCallID)).Scan(&exists)
	if err != nil {
		return nil, fmt.Errorf("load delegated budget replay: %w", err)
	}

	if !exists {
		return nil, nil //nolint:nilnil // No receipt means the mutation has not committed.
	}

	record, err := scanBudget(
		tx.QueryRowContext(ctx, budgetSelect+` WHERE root_session_id = ?`, mutation.RootSessionID),
	)
	if err != nil {
		return nil, fmt.Errorf("load replayed delegated budget: %w", err)
	}

	return record, nil
}
