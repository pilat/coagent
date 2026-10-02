package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/transcript"
)

func observeCommitBudgetTx(ctx context.Context, tx *sql.Tx, c Commit, result *CommitResult) error {
	record, err := scanBudget(tx.QueryRowContext(ctx, budgetSelect+` WHERE root_session_id = ?`, c.RootID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load commit budget: %w", err)
	}
	result.Budget = record
	if record.State == budget.Fired {
		result.BudgetFired = true
	}
	if record.State == budget.Armed {
		reason, delta, err := budgetCrossing(ctx, tx, record, c.At)
		if err != nil {
			return err
		}
		if reason != "" {
			content := fmt.Sprintf("Budget checkpoint reached (%s). Persisted cost: $%.6f. The limiter is no longer armed.", reason, delta)
			message := lastCommitAssistant(c.Messages)
			if c.SessionID == c.RootID && message != nil && message.Content != "" {
				content = message.Content + "\n\n" + content
			}
			owner, err := outputOwner(ctx, tx, c.RootID)
			if errors.Is(err, ErrOutputOwner) || errors.Is(err, ErrOutputNotRoot) {
				owner = ""
			} else if err != nil {
				return err
			}
			_, output, err := fireBudgetTx(ctx, tx, c.RootID, record.Generation, reason, delta, content, owner)
			if err != nil {
				return err
			}
			if output != nil {
				result.Outputs = append(result.Outputs, output)
			}
			record, err = scanBudget(tx.QueryRowContext(ctx, budgetSelect+` WHERE root_session_id = ?`, c.RootID))
			if err != nil {
				return fmt.Errorf("reload fired budget: %w", err)
			}
			result.Budget, result.BudgetFired = record, true
		}
	}
	if result.BudgetFired {
		if message := lastCommitAssistant(c.Messages); message != nil {
			if err := insertBudgetNonExecution(ctx, tx, c.SessionID, message.ToolCalls, c.At); err != nil {
				return err
			}
		}
	}
	return nil
}

func lastCommitAssistant(messages []*transcript.Message) *transcript.Message {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i] != nil && messages[i].Role == assistantRole {
			return messages[i]
		}
	}
	return nil
}
