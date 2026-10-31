package schedule

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

func (s *store) DeliverOneShot(
	ctx context.Context,
	sched *Schedule,
	content string,
	attributes map[string]any,
) (bool, error) {
	return s.deliver(ctx, sched, sessionstore.Input{
		SessionID: sched.sessionID, Source: sessionstore.InputSourceSchedule, Content: content, Attributes: attributes,
	}, func(tx *sql.Tx) (sql.Result, error) {
		return tx.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, sched.id)
	})
}

func (s *store) DeliverSleep(ctx context.Context, sched *Schedule) (bool, error) {
	return s.deliver(ctx, sched, sessionstore.Input{
		SessionID: sched.sessionID, Source: sessionstore.InputSourceCallResult, Content: sched.inputMessage,
		Attributes: map[string]any{"call_id": sched.metadata.ToolCallID, "tool_id": tool.IDSleep},
	}, func(tx *sql.Tx) (sql.Result, error) {
		return tx.ExecContext(ctx, `DELETE FROM schedules WHERE id = ?`, sched.id)
	})
}

func (s *store) DeliverCron(
	ctx context.Context,
	sched *Schedule,
	content string,
	attributes map[string]any,
	occurrence, now time.Time,
) (bool, error) {
	return s.deliver(ctx, sched, sessionstore.Input{
		SessionID: sched.sessionID, Source: sessionstore.InputSourceSchedule, Content: content, Attributes: attributes,
	}, func(tx *sql.Tx) (sql.Result, error) {
		return tx.ExecContext(ctx, `UPDATE schedules SET last_fired_at = ?, fire_count = fire_count + 1
			WHERE id = ? AND (last_fired_at IS NULL OR last_fired_at < ?)`, now.UTC(), sched.id, occurrence.UTC())
	})
}

func (s *store) deliver(
	ctx context.Context,
	sched *Schedule,
	input sessionstore.Input,
	acknowledge func(*sql.Tx) (sql.Result, error),
) (bool, error) {
	applied := false

	err := s.sessions.WithTx(ctx, func(tx *sql.Tx) error {
		result, err := acknowledge(tx)
		if err != nil {
			return fmt.Errorf("acknowledge schedule %d: %w", sched.id, err)
		}

		rows, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("schedule delivery rows: %w", err)
		}

		if rows == 0 {
			return nil
		}

		if input.Source == sessionstore.InputSourceCallResult {
			pending, err := sessionstore.CallPendingTx(ctx, tx, sched.sessionID, sched.metadata.ToolCallID)
			if err != nil {
				return fmt.Errorf("check schedule %d call: %w", sched.id, err)
			}

			if !pending {
				return nil
			}
		}

		enqueued, err := sessionstore.EnqueueTx(ctx, tx, input)
		if err != nil {
			return fmt.Errorf("deliver schedule %d: %w", sched.id, err)
		}

		applied = enqueued.Applied

		return nil
	})
	if err != nil {
		return false, fmt.Errorf("deliver schedule %d transaction: %w", sched.id, err)
	}

	return applied, nil
}
