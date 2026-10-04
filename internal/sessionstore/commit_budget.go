package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

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
	if record.State == budget.Armed {
		if err := observeArmedCommitBudgetTx(ctx, tx, c, result); err != nil {
			return err
		}
	}

	result.BudgetFired = result.Budget.State == budget.Fired

	message := lastCommitAssistant(c.Messages)
	if !result.BudgetFired || message == nil || message.RejectedReason != "" {
		return nil
	}

	return insertBudgetNonExecution(ctx, tx, c.SessionID, message.ToolCalls, c.At)
}

func observeArmedCommitBudgetTx(ctx context.Context, tx *sql.Tx, c Commit, result *CommitResult) error {
	reason, delta, err := budgetCrossing(ctx, tx, result.Budget, c.At)
	if err != nil {
		return err
	}

	if reason == "" {
		return nil
	}

	content := fmt.Sprintf(
		"Budget checkpoint reached (%s). Persisted cost: $%.6f. The limiter is no longer armed.", reason, delta,
	)
	if text := budgetCheckpointText(c); text != "" {
		content = text + "\n\n" + content
	}

	_, output, err := fireBudgetTx(ctx, tx, c.RootID, result.Budget.Generation, reason, delta, content)
	if err != nil {
		return err
	}

	if output != nil {
		result.Outputs = append(result.Outputs, output)
	}

	record, err := scanBudget(tx.QueryRowContext(ctx, budgetSelect+` WHERE root_session_id = ?`, c.RootID))
	if err != nil {
		return fmt.Errorf("reload fired budget: %w", err)
	}

	result.Budget = record

	return nil
}

func budgetCheckpointText(c Commit) string {
	message := lastCommitAssistant(c.Messages)
	if c.SessionID != c.RootID || message == nil || message.RejectedReason != "" {
		return ""
	}
	// Unconfirmed candidates stay private even when their cost fires the budget.
	if candidate := c.Unfired.State.Candidate; candidate != nil && candidate.NextRef >= 0 {
		return ""
	}

	return message.Content
}

func lastCommitAssistant(messages []*transcript.Message) *transcript.Message {
	for _, message := range slices.Backward(messages) {
		if message != nil && message.Role == assistantRole {
			return message
		}
	}

	return nil
}
