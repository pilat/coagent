package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type ModelInputStore interface {
	EnqueueModelInput(
		ctx context.Context,
		sessionID int64,
		rawContent string,
	) (*InboxInput, error)
}

var _ ModelInputStore = (*store)(nil)

//nolint:funlen,gocyclo // One transaction owns episode timing, release arbitration, and inbox insertion.
func (s *store) EnqueueModelInput(
	ctx context.Context,
	sessionID int64,
	rawContent string,
) (*InboxInput, error) {
	if rawContent == "" {
		return nil, errors.New("invalid model input")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin model input: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	var owner string
	var parentID int64
	var status SessionStatus
	var episodeStartedAt sql.NullTime

	err = tx.QueryRowContext(ctx, `SELECT json_extract(attributes, '$.manager_id'), parent_id, status,
			episode_started_at
		FROM sessions WHERE id = ? AND killed_at IS NULL
			AND status NOT IN ('killed', 'terminating', 'stopping')`, sessionID).
		Scan(&owner, &parentID, &status, &episodeStartedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrSessionNotAcceptingInput
	}

	if err != nil {
		return nil, fmt.Errorf("load model input session: %w", err)
	}

	if owner == "" || parentID != 0 {
		return nil, ErrOutputOwner
	}

	now := time.Now().UTC()
	if !episodeStartedAt.Valid || (status != SessionStatusActive && status != SessionStatusSuspended) {
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE sessions SET episode_started_at = ? WHERE id = ?`,
			now,
			sessionID,
		); err != nil {
			return nil, fmt.Errorf("start autonomous episode: %w", err)
		}
	}
	var budgetState, parkPhase sql.NullString

	err = tx.QueryRowContext(ctx, `SELECT state, park_phase FROM session_budgets
		WHERE root_session_id = ?`, sessionID).Scan(&budgetState, &parkPhase)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("load input budget: %w", err)
	}

	if budgetState.String == "fired" {
		if parkPhase.String == "draining" {
			return nil, ErrBudgetConflict
		}

		result, releaseErr := tx.ExecContext(ctx, `UPDATE session_budgets
			SET state = 'released', released_at = ?, released_reason = 'resumed', park_owner = ''
			WHERE root_session_id = ? AND state = 'fired' AND park_phase IN ('requested', 'parked')`,
			now, sessionID)
		if releaseErr != nil {
			return nil, fmt.Errorf("release fired budget for input: %w", releaseErr)
		}

		if releaseErr := requireActivationChanged(result); releaseErr != nil {
			return nil, ErrBudgetConflict
		}
	}

	encoded, err := json.Marshal(map[string]any{managerIDAttribute: owner})
	if err != nil {
		return nil, fmt.Errorf("marshal model input owner: %w", err)
	}

	result, err := tx.ExecContext(ctx, `INSERT INTO session_inbox
		(session_id, source, raw_content, attributes, received_at)
		VALUES (?, 'user', ?, ?, ?)`, sessionID, rawContent, string(encoded), now)
	if err != nil {
		return nil, fmt.Errorf("insert model input: %w", err)
	}

	inputID, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("model input id: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit model input: %w", err)
	}

	return &InboxInput{
		ID: inputID, SessionID: sessionID, Source: InputSourceUser,
		RawContent: rawContent, Attributes: map[string]any{managerIDAttribute: owner},
		ReceivedAt: now, State: InputStatePending,
	}, nil
}
