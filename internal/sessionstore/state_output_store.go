package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// StateOutputStore commits a terminal runner state and its manager-visible
// error as one fact. Ownerless roots and children still receive the state update
// but never acquire an outbox owner.
type StateOutputStore interface {
	UpdateSessionIterationWithOutput(
		ctx context.Context,
		sessionID int64,
		iteration int,
		status SessionStatus,
		content string,
	) (*OutputCommit, error)
}

func (s *store) UpdateSessionIterationWithOutput(
	ctx context.Context,
	sessionID int64,
	iteration int,
	status SessionStatus,
	content string,
) (*OutputCommit, error) {
	if !status.valid() || content == "" {
		return nil, errors.New("invalid session state output")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin session state output: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC()

	result, err := tx.ExecContext(ctx, `UPDATE sessions
		SET iteration = ?, status = ?, updated_at = ?
		WHERE id = ? AND killed_at IS NULL
			AND status NOT IN ('stopping', 'terminating', 'killed')`, iteration, status, now, sessionID)
	if err != nil {
		return nil, fmt.Errorf("update session state: %w", err)
	}

	if err := requireOneSessionUpdate(result, sessionID); err != nil {
		return nil, err
	}

	owner, err := outputOwner(ctx, tx, sessionID)
	if errors.Is(err, ErrOutputOwner) || errors.Is(err, ErrOutputNotRoot) {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("commit session state: %w", err)
		}

		return &OutputCommit{}, nil
	}

	if err != nil {
		return nil, err
	}

	attributes, err := stampMessageOutputAttributes(ctx, tx, sessionID, owner, nil)
	if err != nil {
		return nil, err
	}

	encoded, err := json.Marshal(attributes)
	if err != nil {
		return nil, fmt.Errorf("marshal state output attributes: %w", err)
	}

	result, err = tx.ExecContext(ctx, `INSERT INTO session_outbox
		(session_id, type, content, attributes, created_at, releases_input)
		VALUES (?, 'message_persistent', ?, ?, ?, 1)`, sessionID, content, string(encoded), now)
	if err != nil {
		return nil, fmt.Errorf("insert state output: %w", err)
	}

	outputID, err := result.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("state output id: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit session state output: %w", err)
	}

	return &OutputCommit{OutputID: outputID, OwnerID: owner}, nil
}
