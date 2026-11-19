package sessionstore

import (
	"context"
	"database/sql"
	"fmt"
)

// WatchState records the independent arms and unread signal for one target.
type WatchState struct {
	Subscribed bool
	Armed      bool
	Signalled  bool
}

// WatchFire contains only host-authored text about the target's turn outcome.
type WatchFire struct {
	TargetID int64
	ActorID  int64
	Line     string
	Notice   string
}

// ArmWatchTx preserves the position and reports whether a signal was already unread.
func ArmWatchTx(ctx context.Context, tx *sql.Tx, watcher, target int64, standing bool) (bool, error) {
	var unread bool

	err := tx.QueryRowContext(ctx, `INSERT INTO session_watches
		(watcher_id, target_id, read_through, subscribed, armed)
		VALUES (?, ?, (SELECT COALESCE(MAX(id), 0) FROM session_outbox WHERE session_id = ?), ?, ?)
		ON CONFLICT(watcher_id, target_id) DO UPDATE SET
		subscribed = MAX(session_watches.subscribed, excluded.subscribed),
		armed = MAX(session_watches.armed, excluded.armed) RETURNING signalled`,
		watcher, target, target, standing, !standing).Scan(&unread)
	if err != nil {
		return false, fmt.Errorf("arm session watch: %w", err)
	}

	return unread, nil
}

// ArmWatch adds an arm and reports an unread signal in the same transaction.
func (s *Store) ArmWatch(ctx context.Context, watcher, target int64, standing bool) (bool, error) {
	var unread bool
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		var err error

		unread, err = ArmWatchTx(ctx, tx, watcher, target, standing)

		return err
	})

	return unread, err
}

// Unsubscribe removes the standing arm without changing a pending one-shot.
func (s *Store) Unsubscribe(ctx context.Context, watcher, target int64) (bool, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE session_watches SET subscribed = 0
		WHERE watcher_id = ? AND target_id = ? AND subscribed = 1`, watcher, target)
	if err != nil {
		return false, fmt.Errorf("unsubscribe session watch: %w", err)
	}

	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("count unsubscribed watches: %w", err)
	}

	return changed != 0, nil
}

// WatchStates returns this watcher's arms and unread signals keyed by target.
func (s *Store) WatchStates(ctx context.Context, watcher int64) (map[int64]WatchState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT target_id, subscribed, armed, signalled
		FROM session_watches WHERE watcher_id = ?`, watcher)
	if err != nil {
		return nil, fmt.Errorf("load session watches: %w", err)
	}
	defer rows.Close()

	states := make(map[int64]WatchState)

	for rows.Next() {
		var target int64
		var state WatchState

		if err := rows.Scan(&target, &state.Subscribed, &state.Armed, &state.Signalled); err != nil {
			return nil, fmt.Errorf("scan session watch: %w", err)
		}

		states[target] = state
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate session watches: %w", err)
	}

	return states, nil
}

// FireWatches consumes eligible arms together with their durable completion inputs.
func (s *Store) FireWatches(ctx context.Context, fire WatchFire) (int, error) {
	count := 0
	err := s.WithTx(ctx, func(tx *sql.Tx) error {
		watchers, err := eligibleWatchers(ctx, tx, fire)
		if err != nil {
			return err
		}

		for _, watcher := range watchers {
			applied, err := fireWatch(ctx, tx, watcher, fire)
			if err != nil {
				return err
			}

			if applied {
				count++
			}
		}

		return nil
	})

	return count, err
}

func eligibleWatchers(ctx context.Context, tx *sql.Tx, fire WatchFire) ([]int64, error) {
	_, err := tx.ExecContext(ctx, `UPDATE session_watches SET armed = 0
		WHERE target_id = ? AND armed = 1 AND (signalled = 1 OR watcher_id = ? OR watcher_id IN
		(SELECT id FROM sessions WHERE killed_at IS NOT NULL OR parent_id <> 0
		OR json_type(attributes, '$.orchestrator') IS NOT 'true'))`, fire.TargetID, fire.ActorID)
	if err != nil {
		return nil, fmt.Errorf("consume inactive or signalled watch arms: %w", err)
	}

	rows, err := tx.QueryContext(ctx, `SELECT w.watcher_id FROM session_watches w
		JOIN sessions s ON s.id = w.watcher_id WHERE w.target_id = ?
		AND w.watcher_id <> ?
		AND w.signalled = 0 AND (w.armed = 1 OR w.subscribed = 1)
		AND s.killed_at IS NULL AND s.parent_id = 0
		AND json_type(s.attributes, '$.orchestrator') = 'true' ORDER BY w.watcher_id`, fire.TargetID, fire.ActorID)
	if err != nil {
		return nil, fmt.Errorf("load armed watchers: %w", err)
	}
	defer rows.Close()

	var watchers []int64

	for rows.Next() {
		var watcher int64
		if err := rows.Scan(&watcher); err != nil {
			return nil, fmt.Errorf("scan armed watcher: %w", err)
		}

		watchers = append(watchers, watcher)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate armed watchers: %w", err)
	}

	return watchers, nil
}

func fireWatch(ctx context.Context, tx *sql.Tx, watcher int64, fire WatchFire) (bool, error) {
	var pending bool

	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_inbox
		WHERE session_id = ? AND state = 'pending' AND json_extract(attributes, '$.signal_target_id') = ?)`,
		watcher, fire.TargetID).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("check pending watch signal: %w", err)
	}

	applied := false

	if !pending {
		result, err := EnqueueTx(ctx, tx, Input{
			SessionID: watcher, Source: InputSourceSubagent, Content: fire.Line,
			Attributes: map[string]any{
				InputAttributeSignalTargetID: fire.TargetID,
				InputAttributeSignalNotice:   fire.Notice,
			},
		})
		if err != nil {
			return false, err
		}

		applied = result.Applied
	}

	if pending || applied {
		_, err = tx.ExecContext(ctx, `UPDATE session_watches SET armed = 0, signalled = 1
			WHERE watcher_id = ? AND target_id = ?`, watcher, fire.TargetID)
		if err != nil {
			return false, fmt.Errorf("consume signalled watch: %w", err)
		}
	}

	return applied, nil
}
