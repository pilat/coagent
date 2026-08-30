// nosemgrep: semgrep.coagent-no-preamble-before-package
package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type ProgressFacts struct {
	RootID           int64
	Model            string
	Iteration        int
	Status           SessionStatus
	TodoItems        json.RawMessage
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
	ChildCount       int
	ChildIterations  int
	// ModelInputGeneration and ModelInputBoundary snapshot the causal chain a
	// progress card belongs to; the note query only reads rows after the
	// boundary so an older turn's narration can never serve a newer one.
	ModelInputGeneration int64
	ModelInputBoundary   int64
	LatestModelProgress  string
	EpisodeStartedAt     *time.Time
	LastSemanticOutputAt *time.Time
	MessageWatermark     int64
	OutboxWatermark      int64
	Budget               *BudgetRecord
	Waiting              []ProgressWait
	ActiveSubagents      int
	BackgroundSubagents  int
}

type ProgressWait struct {
	Kind        string
	Description string
	WakeAt      *time.Time
}

type ProgressStore interface {
	CaptureProgress(ctx context.Context, rootID int64) (*ProgressFacts, error)
	ListAutonomousProgressRoots(ctx context.Context) ([]int64, error)
	OutboxWatermark(ctx context.Context, sessionID int64) (int64, error)
	// EnqueueProgressOutput commits one causal progress card: it succeeds only
	// while the captured generation and status still own the session.
	EnqueueProgressOutput(
		ctx context.Context,
		draft OutputDraft,
		expectedGeneration int64,
		expectedStatus SessionStatus,
	) (*OutputCommit, error)
}

var _ ProgressStore = (*store)(nil)

func (s *store) CaptureProgress(ctx context.Context, rootID int64) (*ProgressFacts, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin progress capture: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	facts := &ProgressFacts{RootID: rootID}
	var todos string
	var boundary sql.NullInt64

	err = tx.QueryRowContext(ctx, `SELECT model, iteration, status, todo_items,
		model_input_generation, model_input_boundary
		FROM sessions WHERE id = ? AND parent_id = 0`, rootID).
		Scan(&facts.Model, &facts.Iteration, &facts.Status, &todos,
			&facts.ModelInputGeneration, &boundary)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOutputNotRoot
	}

	if err != nil {
		return nil, fmt.Errorf("load progress root: %w", err)
	}

	facts.TodoItems = json.RawMessage(todos)
	facts.ModelInputBoundary = boundary.Int64

	err = tx.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(json_extract(messages.usage, '$.promptTokens')), 0),
		COALESCE(SUM(json_extract(messages.usage, '$.completionTokens')), 0),
		COALESCE(SUM(messages.cost_usd), 0),
		COALESCE(MAX(messages.id), 0)
		FROM sessions LEFT JOIN messages ON messages.session_id = sessions.id
		WHERE sessions.id = ? OR sessions.root_id = ?`, rootID, rootID).
		Scan(&facts.PromptTokens, &facts.CompletionTokens, &facts.CostUSD, &facts.MessageWatermark)
	if err != nil {
		return nil, fmt.Errorf("load progress tree usage: %w", err)
	}

	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(iteration), 0)
		FROM sessions WHERE root_id = ?`, rootID).Scan(&facts.ChildCount, &facts.ChildIterations); err != nil {
		return nil, fmt.Errorf("load progress child stats: %w", err)
	}

	// The current-generation note is the newest unpublished assistant narration
	// after the generation boundary. A direct reply already has its own durable
	// output and must not be duplicated inside the replaceable progress card.
	if facts.ModelInputBoundary > 0 {
		var latest sql.NullString

		err = tx.QueryRowContext(ctx, `SELECT messages.content FROM messages
			WHERE messages.session_id = ? AND messages.role = 'assistant' AND messages.id > ?
			AND messages.compacted_at IS NULL AND TRIM(COALESCE(messages.content, '')) <> ''
			AND json_type(messages.tool_calls) = 'array' AND json_array_length(messages.tool_calls) > 0
			AND NOT EXISTS (SELECT 1 FROM session_outbox WHERE session_outbox.session_id = messages.session_id
				AND session_outbox.source_key = 'message:' || messages.id || ':reply')
			ORDER BY messages.id DESC LIMIT 1`, rootID, facts.ModelInputBoundary).Scan(&latest)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("load latest model progress: %w", err)
		}

		facts.LatestModelProgress = latest.String
	}

	if err := captureProgressTimes(ctx, tx, facts); err != nil {
		return nil, err
	}

	if err := captureProgressWaiting(ctx, tx, facts); err != nil {
		return nil, err
	}

	budget, err := scanBudget(tx.QueryRowContext(ctx, budgetSelect+` WHERE root_session_id = ?`, rootID))
	if err == nil {
		facts.Budget = budget
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load progress budget: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit progress capture: %w", err)
	}

	return facts, nil
}

func captureProgressWaiting(ctx context.Context, tx *sql.Tx, facts *ProgressFacts) error {
	if err := captureProgressSubagents(ctx, tx, facts); err != nil {
		return err
	}

	return captureProgressSleeps(ctx, tx, facts)
}

//nolint:wsl_v5 // Projection loops keep scan and append adjacent.
func captureProgressSubagents(ctx context.Context, tx *sql.Tx, facts *ProgressFacts) error {
	rows, err := tx.QueryContext(ctx, `SELECT child_id, blocking FROM subagent_links
		WHERE parent_id = ? AND state IN ('spawned', 'running') ORDER BY child_id`, facts.RootID)
	if err != nil {
		return fmt.Errorf("load progress subagents: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var childID int64

		var blocking bool
		if err := rows.Scan(&childID, &blocking); err != nil {
			return fmt.Errorf("scan progress subagent: %w", err)
		}

		facts.ActiveSubagents++
		if blocking {
			facts.Waiting = append(facts.Waiting, ProgressWait{
				Kind: "subagent", Description: fmt.Sprintf("subagent #%d", childID),
			})
		} else {
			facts.BackgroundSubagents++
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
	var semantic time.Time

	err := tx.QueryRowContext(ctx, `SELECT episode_started_at
		FROM sessions WHERE id = ?`, facts.RootID).Scan(&episode)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load progress episode start: %w", err)
	}

	if episode.Valid {
		facts.EpisodeStartedAt = &episode.Time
	}

	err = tx.QueryRowContext(ctx, `SELECT created_at FROM session_outbox WHERE session_id = ?
		AND type IN ('message_replaceable', 'message_persistent') ORDER BY id DESC LIMIT 1`, facts.RootID).
		Scan(&semantic)
	if err == nil {
		facts.LastSemanticOutputAt = &semantic
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("load last semantic output: %w", err)
	}

	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM session_outbox
		WHERE session_id = ?`, facts.RootID).Scan(&facts.OutboxWatermark); err != nil {
		return fmt.Errorf("load progress outbox watermark: %w", err)
	}

	return nil
}

// OutboxWatermark is the light single-fact re-read producers use to detect a
// semantic output committed after their snapshot capture.
func (s *store) OutboxWatermark(ctx context.Context, sessionID int64) (int64, error) {
	var watermark int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM session_outbox
		WHERE session_id = ?`, sessionID).Scan(&watermark); err != nil {
		return 0, fmt.Errorf("load outbox watermark: %w", err)
	}

	return watermark, nil
}

func (s *store) ListAutonomousProgressRoots(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT root.id FROM sessions root
		LEFT JOIN sessions child ON child.root_id = root.id
		WHERE root.parent_id = 0 AND root.killed_at IS NULL
			AND json_type(root.attributes, '$.manager_id') = 'text'
			AND json_extract(root.attributes, '$.manager_id') <> ''
			AND root.episode_started_at IS NOT NULL
			AND root.status NOT IN ('stopping', 'stopped', 'terminating', 'killed')
			AND (root.status IN ('active', 'suspended') OR child.status IN ('active', 'suspended'))
		ORDER BY root.id`)
	if err != nil {
		return nil, fmt.Errorf("list progress roots: %w", err)
	}
	defer rows.Close()
	var ids []int64

	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan autonomous progress root: %w", err)
		}

		ids = append(ids, id)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate autonomous progress roots: %w", err)
	}

	return ids, nil
}
