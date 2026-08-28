package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"
)

const budgetToolNotExecuted = "Not executed because the budget checkpoint fired."

type BudgetedResponse struct {
	SessionID  int64
	RootID     int64
	Message    *StoredMessage
	ObservedAt time.Time
}

type BudgetedResponseResult struct {
	MessageID int64
	Fired     bool
	Budget    *BudgetRecord
}

type BudgetResponseStore interface {
	InsertBudgetedResponse(ctx context.Context, response BudgetedResponse) (*BudgetedResponseResult, error)
}

var _ BudgetResponseStore = (*store)(nil)

//nolint:gocyclo,wsl_v5 // Transaction phases intentionally remain visually compact.
func (s *store) InsertBudgetedResponse(
	ctx context.Context,
	response BudgetedResponse,
) (*BudgetedResponseResult, error) {
	if response.SessionID <= 0 || response.RootID <= 0 || response.Message == nil ||
		response.Message.Role != "assistant" {
		return nil, ErrBudgetConflict
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin budgeted response: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	messageID, err := insertMessageWith(ctx, tx, response.SessionID, response.Message)
	if err != nil {
		return nil, err
	}

	record, err := scanBudget(tx.QueryRowContext(ctx, budgetSelect+` WHERE root_session_id = ?`, response.RootID))
	if err == nil && record.State == BudgetFired {
		now := response.ObservedAt.UTC()
		if response.ObservedAt.IsZero() {
			now = time.Now().UTC()
		}
		if err := insertBudgetNonExecution(ctx, tx, response.SessionID, response.Message.ToolCalls, now); err != nil {
			return nil, err
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return nil, fmt.Errorf("commit response behind fired budget: %w", commitErr)
		}

		return &BudgetedResponseResult{MessageID: messageID, Fired: true, Budget: record}, nil
	}
	if errors.Is(err, sql.ErrNoRows) || (err == nil && record.State != BudgetArmed) {
		if commitErr := tx.Commit(); commitErr != nil {
			return nil, fmt.Errorf("commit unarmed budgeted response: %w", commitErr)
		}

		return &BudgetedResponseResult{MessageID: messageID, Budget: record}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load response budget: %w", err)
	}

	observedAt := response.ObservedAt.UTC()
	if response.ObservedAt.IsZero() {
		observedAt = time.Now().UTC()
	}

	reason, delta, err := budgetCrossing(ctx, tx, record, observedAt)
	if err != nil {
		return nil, err
	}
	if reason == "" {
		if commitErr := tx.Commit(); commitErr != nil {
			return nil, fmt.Errorf("commit budgeted response: %w", commitErr)
		}

		return &BudgetedResponseResult{MessageID: messageID, Budget: record}, nil
	}

	if err := fireBudgetedResponse(ctx, tx, response, record, reason, delta, observedAt); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit crossing budgeted response: %w", err)
	}

	record.State = BudgetFired
	record.FiredReason = reason
	record.FiredAt = &observedAt
	record.ObservedCostUSD = &delta
	record.ParkPhase = "requested"
	record.ParkOwner = fmt.Sprintf("budget:%d:%d", response.RootID, record.Generation)

	return &BudgetedResponseResult{MessageID: messageID, Fired: true, Budget: record}, nil
}

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

//nolint:wsl_v5 // Fire, non-execution, and checkpoint form one transaction.
func fireBudgetedResponse(
	ctx context.Context,
	tx *sql.Tx,
	response BudgetedResponse,
	record *BudgetRecord,
	reason string,
	delta float64,
	now time.Time,
) error {
	parkOwner := fmt.Sprintf("budget:%d:%d", response.RootID, record.Generation)
	result, err := tx.ExecContext(ctx, `UPDATE session_budgets SET state = 'fired', fired_at = ?,
		fired_reason = ?, observed_cost_usd = ?, park_phase = 'requested', park_owner = ?
		WHERE root_session_id = ? AND generation = ? AND state = 'armed'`,
		now, reason, delta, parkOwner, response.RootID, record.Generation)
	if err != nil {
		return fmt.Errorf("fire response budget: %w", err)
	}
	if err := requireActivationChanged(result); err != nil {
		return ErrBudgetConflict
	}

	if err := insertBudgetNonExecution(ctx, tx, response.SessionID, response.Message.ToolCalls, now); err != nil {
		return err
	}

	owner, err := outputOwner(ctx, tx, response.RootID)
	if err != nil {
		return err
	}
	content := fmt.Sprintf(
		"Budget checkpoint reached (%s). Persisted cost: $%.6f. The limiter is no longer armed.", reason, delta,
	)
	if response.SessionID == response.RootID && response.Message.Content != "" {
		content = response.Message.Content + "\n\n" + content
	}

	_, err = insertMessageOutput(ctx, tx, response.RootID, owner, content,
		fmt.Sprintf("budget:%d:checkpoint", record.Generation), now, true)

	return err
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
		_, err := insertToolResultOnce(ctx, tx, sessionID, &StoredMessage{
			Role: "tool", Content: budgetToolNotExecuted, ToolCallID: call.ID,
			ToolName: call.Name, CreatedAt: now,
		})
		if err != nil {
			return fmt.Errorf("insert budget non-execution result: %w", err)
		}
	}

	return nil
}
