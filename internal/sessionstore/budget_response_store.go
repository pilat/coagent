package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/pilat/coagent/internal/transcript"
)

const budgetToolNotExecuted = "Not executed because the budget checkpoint fired."

//nolint:wsl_v5 // The usage query directly precedes threshold selection.
func budgetCrossing(
	ctx context.Context,
	tx *sql.Tx,
	record *BudgetRecord,
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

	return budgetCrossingReason(record, delta, observedAt), delta, nil
}

// budgetCrossingReason pins the plan's precedence: duration wins iff its
// deadline is crossed at the observation; otherwise cost, compared at the
// six-decimal precision the receipts use.
func budgetCrossingReason(record *BudgetRecord, delta float64, observedAt time.Time) string {
	if record.DurationSeconds != nil &&
		!observedAt.Before(record.ArmedAt.Add(time.Duration(*record.DurationSeconds)*time.Second)) {
		return "duration"
	}

	if record.CostLimitUSD != nil && roundCostUSD(delta) >= roundCostUSD(*record.CostLimitUSD) {
		return "cost"
	}

	return ""
}

func roundCostUSD(value float64) float64 {
	return math.Round(value*1_000_000) / 1_000_000
}

//nolint:wsl_v5 // Decode and insertion are one deterministic replay boundary.
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
			ToolName: call.Name, CreatedAt: now,
		})
		if err != nil {
			return fmt.Errorf("insert budget non-execution result: %w", err)
		}
	}

	return nil
}
