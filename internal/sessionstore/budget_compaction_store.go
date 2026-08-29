package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type BudgetedCompaction struct {
	SessionID    int64
	RootID       int64
	InputID      int64
	CompactedIDs []int64
	Entries      []CompactionEntry
	ObservedAt   time.Time
}

type BudgetedCompactionResult struct {
	MessageIDs []int64
	Fired      bool
	Budget     *BudgetRecord
}

type BudgetCompactionStore interface {
	ReplaceCompactedMessagesBudgeted(
		ctx context.Context,
		compaction BudgetedCompaction,
	) (*BudgetedCompactionResult, error)
}

var _ BudgetCompactionStore = (*store)(nil)

//nolint:wsl_v5 // Replacement, command settlement, and budget fire are one transaction.
func (s *store) ReplaceCompactedMessagesBudgeted(
	ctx context.Context,
	compaction BudgetedCompaction,
) (*BudgetedCompactionResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin budgeted compaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := compaction.ObservedAt.UTC()
	if compaction.ObservedAt.IsZero() {
		now = time.Now().UTC()
	}

	ids, err := replaceCompactedMessagesTx(
		ctx, tx, compaction.SessionID, compaction.CompactedIDs, compaction.Entries, now,
	)
	if err != nil {
		return nil, err
	}
	if compaction.InputID > 0 {
		if err := settleBudgetedCompactionInput(ctx, tx, compaction, now); err != nil {
			return nil, err
		}
	}

	record, err := scanBudget(tx.QueryRowContext(ctx, budgetSelect+` WHERE root_session_id = ?`, compaction.RootID))
	if errors.Is(err, sql.ErrNoRows) || (err == nil && record.State == BudgetReleased) {
		if commitErr := tx.Commit(); commitErr != nil {
			return nil, fmt.Errorf("commit unarmed compaction: %w", commitErr)
		}

		return &BudgetedCompactionResult{MessageIDs: ids, Budget: record}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load compaction budget: %w", err)
	}
	if record.State == BudgetFired {
		if commitErr := tx.Commit(); commitErr != nil {
			return nil, fmt.Errorf("commit compaction behind fired budget: %w", commitErr)
		}

		return &BudgetedCompactionResult{MessageIDs: ids, Fired: true, Budget: record}, nil
	}

	reason, delta, err := budgetCrossing(ctx, tx, record, now)
	if err != nil {
		return nil, err
	}
	if reason != "" {
		if err := fireCompactionBudget(ctx, tx, record, reason, delta, now); err != nil {
			return nil, err
		}
		record.State = BudgetFired
		record.FiredReason = reason
		record.ParkPhase = budgetParkRequestedState
		record.ParkOwner = fmt.Sprintf("budget:%d:%d", record.RootSessionID, record.Generation)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit budgeted compaction: %w", err)
	}

	return &BudgetedCompactionResult{MessageIDs: ids, Fired: reason != "", Budget: record}, nil
}

//nolint:wsl_v5 // Command settlement is one ordered transaction phase.
func settleBudgetedCompactionInput(
	ctx context.Context,
	tx *sql.Tx,
	compaction BudgetedCompaction,
	now time.Time,
) error {
	input, err := loadInboxInput(ctx, tx, compaction.InputID)
	if err != nil {
		return err
	}
	if input.State != InputStatePending || input.SessionID != compaction.SessionID {
		return fmt.Errorf("%w: compact input %d", ErrInputResolved, compaction.InputID)
	}
	owner, err := outputOwner(ctx, tx, compaction.SessionID)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE session_inbox SET state = 'handled', resolved_at = ?,
		resolution_reason = 'compact command' WHERE id = ? AND state = 'pending'`, now, compaction.InputID)
	if err != nil {
		return fmt.Errorf("handle budgeted compaction input: %w", err)
	}
	if err := requireOnePendingResolution(ctx, tx, result, compaction.InputID); err != nil {
		return err
	}
	attributes, err := stampMessageOutputAttributes(ctx, tx, compaction.SessionID, owner, nil)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(attributes)
	if err != nil {
		return fmt.Errorf("marshal compaction output attributes: %w", err)
	}
	content := "✅ Context compacted"
	key := fmt.Sprintf("input:%d:compact:succeeded", compaction.InputID)
	_, err = tx.ExecContext(ctx, `INSERT INTO session_outbox
		(session_id, type, content, attributes, source_key, fingerprint, created_at)
		VALUES (?, 'message_persistent', ?, ?, ?, ?, ?)`, compaction.SessionID, content,
		string(encoded), key, outputFingerprint(OutputMessagePersistent, content, compaction.SessionID, nil), now)
	if err != nil {
		return fmt.Errorf("insert budgeted compaction outcome: %w", err)
	}

	return nil
}

//nolint:wsl_v5 // Fire state and checkpoint intent must stay adjacent.
func fireCompactionBudget(
	ctx context.Context,
	tx *sql.Tx,
	record *BudgetRecord,
	reason string,
	delta float64,
	now time.Time,
) error {
	parkOwner := fmt.Sprintf("budget:%d:%d", record.RootSessionID, record.Generation)
	result, err := tx.ExecContext(ctx, `UPDATE session_budgets SET state = 'fired', fired_at = ?,
		fired_reason = ?, observed_cost_usd = ?, park_phase = 'requested', park_owner = ?
		WHERE root_session_id = ? AND generation = ? AND state = 'armed'`, now, reason, delta,
		parkOwner, record.RootSessionID, record.Generation)
	if err != nil {
		return fmt.Errorf("fire compaction budget: %w", err)
	}
	if err := requireActivationChanged(result); err != nil {
		return ErrBudgetConflict
	}
	owner, err := outputOwner(ctx, tx, record.RootSessionID)
	if err != nil {
		return err
	}
	content := fmt.Sprintf(
		"Budget checkpoint reached (%s). Persisted cost: $%.6f. The limiter is no longer armed.", reason, delta,
	)
	_, err = insertMessageOutput(ctx, tx, record.RootSessionID, owner, content,
		fmt.Sprintf("budget:%d:checkpoint", record.Generation), now, true)

	return err
}
