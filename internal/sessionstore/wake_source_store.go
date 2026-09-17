package sessionstore

import (
	"context"
	"fmt"
)

// WakeSourceStore projects whether the exact session owns a durable
// background wake source: an advertised running process, an undelivered
// non-blocking child link in a state promising automatic delivery, or pending
// process/subagent inbox input. Stopped and killed links are not wake sources.
type WakeSourceStore interface {
	// HasBackgroundWakeSource reports a wake source for one exact session.
	// Producer ledgers read before the inbox so their atomic
	// terminal-to-inbox transition cannot disappear between observations.
	HasBackgroundWakeSource(ctx context.Context, sessionID int64) (bool, error)
}

var _ WakeSourceStore = (*store)(nil)

// BackgroundObligationStore projects the root tree's durable background
// obligations for budget retention, sharing the wake-source predicate at
// subtree scope.
type BackgroundObligationStore interface {
	HasBackgroundObligationByRoot(ctx context.Context, rootID int64) (bool, error)
}

var _ BackgroundObligationStore = (*store)(nil)

func (s *store) HasBackgroundObligationByRoot(ctx context.Context, rootID int64) (bool, error) {
	var advertised int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM background_processes
		WHERE root_session_id = ? AND state = 'running' AND advertised_at IS NOT NULL`,
		rootID).Scan(&advertised); err != nil {
		return false, fmt.Errorf("list advertised running processes: %w", err)
	}

	if advertised > 0 {
		return true, nil
	}

	var undelivered int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM subagent_links
		WHERE parent_id IN (SELECT id FROM sessions WHERE id = ? OR root_id = ?)
			AND delivered_at IS NULL AND blocking = FALSE
			AND state IN ('spawned', 'running', 'completed', 'error')`,
		rootID, rootID).Scan(&undelivered); err != nil {
		return false, fmt.Errorf("list undelivered background links: %w", err)
	}

	if undelivered > 0 {
		return true, nil
	}

	return s.HasPendingAsyncInputByRoot(ctx, rootID)
}

func (s *store) HasBackgroundWakeSource(ctx context.Context, sessionID int64) (bool, error) {
	var advertised int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM background_processes
		WHERE session_id = ? AND state = 'running' AND advertised_at IS NOT NULL`,
		sessionID).Scan(&advertised); err != nil {
		return false, fmt.Errorf("list advertised running processes: %w", err)
	}

	if advertised > 0 {
		return true, nil
	}

	var undelivered int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM subagent_links
		WHERE parent_id = ? AND delivered_at IS NULL AND blocking = FALSE
			AND state IN ('spawned', 'running', 'completed', 'error')`,
		sessionID).Scan(&undelivered); err != nil {
		return false, fmt.Errorf("list undelivered background links: %w", err)
	}

	if undelivered > 0 {
		return true, nil
	}

	var pending bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM session_inbox
		WHERE session_id = ? AND state = 'pending' AND source IN ('process', 'subagent')
	)`, sessionID).Scan(&pending); err != nil {
		return false, fmt.Errorf("query pending async input: %w", err)
	}

	return pending, nil
}
