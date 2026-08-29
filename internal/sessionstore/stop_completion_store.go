package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// StopTerminalContent is the durable terminal fact of an explicit /stop.
const StopTerminalContent = "⏸️ Session stopped"

// ErrStopNotStopping reports a terminal stop commit against a root that is not
// (or no longer) parked in its stopping fence.
var ErrStopNotStopping = errors.New("explicit stop requires a stopping root")

// InterruptedExplicitStop is one handled /stop whose visible terminal fact is
// still owed: it has a started (or legacy :result) output and no completion.
type InterruptedExplicitStop struct {
	SessionID int64
	InputID   int64
}

// StopCompletionStore commits the explicit stop's terminal fact atomically with
// the root's final status and the armed-budget release.
type StopCompletionStore interface {
	CompleteExplicitStop(ctx context.Context, rootID, inputID int64) (*OutputCommit, error)
	// SelectInterruptedExplicitStops lists roots whose newest qualifying /stop
	// input still owes its terminal output. Startup may finish only these.
	SelectInterruptedExplicitStops(ctx context.Context) ([]InterruptedExplicitStop, error)
}

var _ StopCompletionStore = (*store)(nil)

// CompleteExplicitStop is the one sanctioned terminal transaction of an
// explicit /stop: it releases an armed budget with reason `stopped`, moves the
// root from `stopping` to `stopped`, and inserts the persistent releasing
// completion output — all or nothing. Failure leaves the root stopping and
// publishes no success. Re-running it after success is a no-op returning the
// originally stored completion row.
func (s *store) CompleteExplicitStop(
	ctx context.Context,
	rootID, inputID int64,
) (*OutputCommit, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin explicit stop completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()

	if _, err := tx.ExecContext(ctx, `UPDATE session_budgets
		SET state = 'released', released_at = ?, released_reason = 'stopped', park_owner = ''
		WHERE root_session_id = ? AND state = 'armed'`, now, rootID); err != nil {
		return nil, fmt.Errorf("release armed budget for stop: %w", err)
	}

	result, err := tx.ExecContext(ctx, `UPDATE sessions
		SET status = 'stopped', updated_at = ?
		WHERE id = ? AND status IN ('stopping', 'stopped')`, now, rootID)
	if err != nil {
		return nil, fmt.Errorf("finalize stopping root: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("finalize stopping root rows affected: %w", err)
	}

	if affected != 1 {
		return nil, fmt.Errorf("%w: session %d", ErrStopNotStopping, rootID)
	}

	owner, err := outputOwner(ctx, tx, rootID)
	if err != nil {
		return nil, err
	}

	attributes, err := stampMessageOutputAttributes(ctx, tx, rootID, owner, nil)
	if err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(attributes)
	if err != nil {
		return nil, fmt.Errorf("marshal stop completion attributes: %w", err)
	}

	key := fmt.Sprintf("input:%d:stop:completed", inputID)
	fingerprint := outputFingerprintWithRelease(
		OutputMessagePersistent, StopTerminalContent, rootID, nil, true,
	)

	result, err = tx.ExecContext(ctx, `
		INSERT INTO session_outbox (session_id, type, content, attributes, source_key, fingerprint, created_at, releases_input)
		VALUES (?, 'message_persistent', ?, ?, ?, ?, ?, 1)
		ON CONFLICT DO NOTHING`,
		rootID, StopTerminalContent, string(encoded), key, fingerprint, now)
	if err != nil {
		return nil, fmt.Errorf("insert stop completion output: %w", err)
	}

	outputID, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("stop completion output id: %w", err)
	}

	if inserted, insertErr := result.RowsAffected(); insertErr != nil || inserted == 0 {
		// The row already exists: replay must return the originally stored
		// completion, not whatever rowid the connection last handed out.
		err = tx.QueryRowContext(ctx, `SELECT id FROM session_outbox
			WHERE session_id = ? AND source_key = ?`, rootID, key).Scan(&outputID)
		if err != nil {
			return nil, fmt.Errorf("load stored stop completion: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit explicit stop completion: %w", err)
	}

	return &OutputCommit{OutputID: outputID, OwnerID: owner}, nil
}

func (s *store) SelectInterruptedExplicitStops(
	ctx context.Context,
) ([]InterruptedExplicitStop, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT MAX(i.id), i.session_id
		FROM session_inbox i
		WHERE i.state = 'handled' AND i.resolution_reason = 'stop'
			AND EXISTS (
				SELECT 1 FROM session_outbox o
				WHERE o.session_id = i.session_id
					AND (o.source_key = 'input:' || i.id || ':stop:started'
						OR o.source_key = 'input:' || i.id || ':stop:result')
			)
			AND NOT EXISTS (
				SELECT 1 FROM session_outbox c
				WHERE c.session_id = i.session_id
					AND c.source_key = 'input:' || i.id || ':stop:completed'
			)
		GROUP BY i.session_id
		ORDER BY i.session_id`)
	if err != nil {
		return nil, fmt.Errorf("list interrupted explicit stops: %w", err)
	}
	defer rows.Close()

	var stops []InterruptedExplicitStop

	for rows.Next() {
		var stop InterruptedExplicitStop
		if err := rows.Scan(&stop.InputID, &stop.SessionID); err != nil {
			return nil, fmt.Errorf("scan interrupted explicit stop: %w", err)
		}

		stops = append(stops, stop)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate interrupted explicit stops: %w", err)
	}

	return stops, nil
}
