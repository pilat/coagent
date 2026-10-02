package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/transcript"
)

const budgetToolNotExecuted = "Not executed because the budget checkpoint fired."

func budgetCrossing(
	ctx context.Context,
	tx *sql.Tx,
	record *budget.Record,
	observedAt time.Time,
) (string, float64, error) {
	var cost float64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(messages.cost_usd), 0)
		FROM sessions LEFT JOIN sessions tree ON tree.id = sessions.id OR tree.root_id = sessions.id
		LEFT JOIN messages ON messages.session_id = tree.id WHERE sessions.id = ?`, record.RootSessionID).
		Scan(&cost)
	if err != nil {
		return "", 0, fmt.Errorf("load crossing tree cost: %w", err)
	}

	delta := cost - record.BaselineCostUSD

	return budget.CrossingReason(record, delta, observedAt), delta, nil
}

func insertBudgetNonExecution(
	ctx context.Context,
	tx *sql.Tx,
	sessionID int64,
	raw json.RawMessage,
	now time.Time,
) error {
	var calls []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, &calls); err != nil {
		return fmt.Errorf("decode crossing tool calls: %w", err)
	}
	for _, call := range calls {
		_, err := insertToolResultOnce(ctx, tx, sessionID, &transcript.Message{
			Role: "tool", Content: budgetToolNotExecuted, ToolCallID: call.ID,
			ToolName: call.Name, ToolError: true, CreatedAt: now,
		})
		if err != nil {
			return fmt.Errorf("insert budget non-execution result: %w", err)
		}
	}

	return nil
}
