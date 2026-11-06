package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/budget"
)

type ProgressFacts struct {
	RootID              int64
	Model               string
	Iteration           int
	Status              SessionStatus
	TodoItems           json.RawMessage
	PromptTokens        int
	CompletionTokens    int
	CostUSD             float64
	ChildCount          int
	ChildIterations     int
	EpisodeStartedAt    *time.Time
	Budget              *budget.Record
	Waiting             []ProgressWait
	ActiveSubagents     int
	BackgroundSubagents int
	BackgroundProcesses []ProcessProgress
}

// ProcessProgress projects one live background Bash process into status
// output. It never carries command text or captured output bytes.
type ProcessProgress struct {
	ProcessID  string
	OwnerID    int64
	IsRoot     bool
	State      string
	StartedAt  time.Time
	DeadlineAt time.Time
	OutputSize int64
}

type ProgressWait struct {
	Kind        string
	Description string
	WakeAt      *time.Time
}

func (s *Store) CaptureProgress(ctx context.Context, rootID int64) (*ProgressFacts, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin progress capture: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	facts, err := captureProgressTx(ctx, tx, rootID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit progress capture: %w", err)
	}

	return facts, nil
}

func captureProgressTx(ctx context.Context, tx *sql.Tx, rootID int64) (*ProgressFacts, error) {
	facts := &ProgressFacts{RootID: rootID}
	if err := captureProgressRoot(ctx, tx, facts); err != nil {
		return nil, err
	}

	if err := tx.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(json_extract(messages.usage, '$.promptTokens')), 0),
		COALESCE(SUM(json_extract(messages.usage, '$.completionTokens')), 0),
		COALESCE(SUM(messages.cost_usd), 0)
		FROM sessions LEFT JOIN messages ON messages.session_id = sessions.id
		WHERE sessions.id = ? OR sessions.root_id = ?`, rootID, rootID).
		Scan(&facts.PromptTokens, &facts.CompletionTokens, &facts.CostUSD); err != nil {
		return nil, fmt.Errorf("load progress tree usage: %w", err)
	}

	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(iteration), 0)
		FROM sessions WHERE root_id = ?`, rootID).Scan(&facts.ChildCount, &facts.ChildIterations); err != nil {
		return nil, fmt.Errorf("load progress child stats: %w", err)
	}

	if err := captureProgressTimes(ctx, tx, facts); err != nil {
		return nil, err
	}

	if err := captureProgressWaiting(ctx, tx, facts); err != nil {
		return nil, err
	}

	if err := captureProgressProcesses(ctx, tx, facts); err != nil {
		return nil, err
	}

	record, err := scanBudget(tx.QueryRowContext(ctx, budgetSelect+` WHERE root_session_id = ?`, rootID))
	if err == nil {
		facts.Budget = record
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load progress budget: %w", err)
	}

	return facts, nil
}

func captureProgressRoot(ctx context.Context, tx *sql.Tx, facts *ProgressFacts) error {
	var todos string

	err := tx.QueryRowContext(ctx, `SELECT model, iteration, status, todo_items
		FROM sessions WHERE id = ? AND parent_id = 0`, facts.RootID).
		Scan(&facts.Model, &facts.Iteration, &facts.Status, &todos)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrOutputNotRoot
	}

	if err != nil {
		return fmt.Errorf("load progress root: %w", err)
	}

	facts.TodoItems = json.RawMessage(todos)

	return nil
}

func captureProgressWaiting(ctx context.Context, tx *sql.Tx, facts *ProgressFacts) error {
	if err := captureProgressSubagents(ctx, tx, facts); err != nil {
		return err
	}

	return captureProgressSleeps(ctx, tx, facts)
}

//nolint:wsl_v5 // Projection loops keep scan and append adjacent.
func captureProgressSubagents(ctx context.Context, tx *sql.Tx, facts *ProgressFacts) error {
	rows, err := tx.QueryContext(ctx, `SELECT links.parent_id, links.child_id, links.blocking FROM subagent_links links
		JOIN sessions child ON child.id = links.child_id
		WHERE child.root_id = ? AND links.state IN ('spawned', 'running') ORDER BY links.child_id`, facts.RootID)
	if err != nil {
		return fmt.Errorf("load progress subagents: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var parentID, childID int64

		var blocking bool
		if err := rows.Scan(&parentID, &childID, &blocking); err != nil {
			return fmt.Errorf("scan progress subagent: %w", err)
		}

		facts.ActiveSubagents++
		if !blocking {
			facts.BackgroundSubagents++
			continue
		}

		if parentID == facts.RootID {
			facts.Waiting = append(facts.Waiting, ProgressWait{
				Kind: "subagent", Description: fmt.Sprintf("subagent #%d", childID),
			})
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate progress subagents: %w", err)
	}

	return nil
}

//nolint:wsl_v5 // Projection loops keep scan and append adjacent.
func captureProgressSleeps(ctx context.Context, tx *sql.Tx, facts *ProgressFacts) error {
	sleeps, err := tx.QueryContext(ctx, `SELECT one_shot_at, json_extract(metadata, '$.reason')
		FROM schedules WHERE session_id = ? AND one_shot_at IS NOT NULL
			AND json_type(metadata, '$.tool_call_id') = 'text'
			AND json_extract(metadata, '$.tool_call_id') <> '' ORDER BY one_shot_at, id`, facts.RootID)
	if err != nil {
		return fmt.Errorf("load progress sleeps: %w", err)
	}
	defer sleeps.Close()
	for sleeps.Next() {
		var wake time.Time
		var reason sql.NullString
		if err := sleeps.Scan(&wake, &reason); err != nil {
			return fmt.Errorf("scan progress sleep: %w", err)
		}
		facts.Waiting = append(facts.Waiting, ProgressWait{
			Kind: "sleep", Description: reason.String, WakeAt: &wake,
		})
	}

	if err := sleeps.Err(); err != nil {
		return fmt.Errorf("iterate progress sleeps: %w", err)
	}

	return nil
}

func captureProgressTimes(ctx context.Context, tx *sql.Tx, facts *ProgressFacts) error {
	var episode sql.NullTime

	err := tx.QueryRowContext(ctx, `SELECT episode_started_at
		FROM sessions WHERE id = ?`, facts.RootID).Scan(&episode)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load progress episode start: %w", err)
	}

	if episode.Valid {
		facts.EpisodeStartedAt = &episode.Time
	}

	return nil
}

// captureProgressProcesses projects advertised running processes owned by the
// root tree. Unadvertised foreground candidates stay invisible.
func captureProgressProcesses(ctx context.Context, tx *sql.Tx, facts *ProgressFacts) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, session_id, session_id = root_session_id, state, created_at, deadline_at, output_size
		FROM background_processes
		WHERE root_session_id = ? AND state = 'running' AND advertised_at IS NOT NULL
		ORDER BY created_at`, facts.RootID)
	if err != nil {
		return fmt.Errorf("load progress background processes: %w", err)
	}

	defer rows.Close()

	for rows.Next() {
		var process ProcessProgress

		var isRoot bool

		if err := rows.Scan(
			&process.ProcessID, &process.OwnerID, &isRoot, &process.State,
			&process.StartedAt, &process.DeadlineAt, &process.OutputSize,
		); err != nil {
			return fmt.Errorf("scan progress background process: %w", err)
		}

		process.IsRoot = isRoot
		facts.BackgroundProcesses = append(facts.BackgroundProcesses, process)
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate progress background processes: %w", err)
	}

	return nil
}
