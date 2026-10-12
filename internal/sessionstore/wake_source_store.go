package sessionstore

import (
	"context"
	"fmt"
	"time"
)

// LiveProcess is an advertised running background process of one exact session.
type LiveProcess struct {
	ID         string
	ToolCallID string
	CreatedAt  time.Time
}

func (s *Store) HasBackgroundObligationByRoot(ctx context.Context, rootID int64) (bool, error) {
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

func (s *Store) HasBackgroundWakeSource(ctx context.Context, sessionID int64) (bool, error) {
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

// ListLiveProcesses returns the session's own advertised running processes, oldest first.
func (s *Store) ListLiveProcesses(ctx context.Context, sessionID int64) ([]LiveProcess, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, tool_call_id, created_at FROM background_processes
		WHERE session_id = ? AND state = 'running' AND advertised_at IS NOT NULL
		ORDER BY created_at, id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("list live processes: %w", err)
	}

	defer rows.Close()

	var live []LiveProcess

	for rows.Next() {
		var process LiveProcess
		if err := rows.Scan(&process.ID, &process.ToolCallID, &process.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan live process: %w", err)
		}

		live = append(live, process)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate live processes: %w", err)
	}

	return live, nil
}

// HasPendingBackgroundWait preserves timer admission across all undelivered child links.
func (s *Store) HasPendingBackgroundWait(ctx context.Context, sessionID int64) (bool, error) {
	var pending bool

	err := s.db.QueryRowContext(ctx, `SELECT
		EXISTS(SELECT 1 FROM subagent_links WHERE parent_id = ? AND delivered_at IS NULL)
		OR EXISTS(SELECT 1 FROM background_processes
			WHERE session_id = ? AND state = 'running' AND advertised_at IS NOT NULL)`,
		sessionID, sessionID).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("query pending background wait: %w", err)
	}

	return pending, nil
}
