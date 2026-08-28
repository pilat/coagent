//nolint:wrapcheck // Store scanners attach operation context at their callers.; nosemgrep: semgrep.coagent-no-preamble-before-package
package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type ActivationState string

const (
	ActivationPending  ActivationState = "pending"
	ActivationConsumed ActivationState = "consumed"
	ActivationExpired  ActivationState = "expired"
)

var (
	ErrActivationNotFound = errors.New("tool activation not found")
	ErrActivationConflict = errors.New("tool activation conflict")
)

type ActivationDraft struct {
	ToolID  string
	Command string
}

type ToolActivation struct {
	InputID    int64
	SessionID  int64
	ToolID     string
	Command    string
	State      ActivationState
	ToolCallID string
	CreatedAt  time.Time
	ResolvedAt *time.Time
}

type ActivationStore interface {
	PromoteInputWithActivation(
		ctx context.Context,
		inputID int64,
		preparedContent string,
		activation ActivationDraft,
	) (*StoredMessage, *ToolActivation, error)
	PendingActivation(ctx context.Context, sessionID int64) (*ToolActivation, error)
	CurrentActivation(ctx context.Context, sessionID int64) (*ToolActivation, error)
	ConsumeActivation(
		ctx context.Context,
		inputID, sessionID int64,
		toolID, command, toolCallID string,
	) (*ToolActivation, error)
	ExpireActivation(ctx context.Context, inputID, sessionID int64) (*ToolActivation, error)
	ExpireActivationWithOutput(
		ctx context.Context,
		inputID, sessionID int64,
		content string,
	) (*ToolActivation, *OutputCommit, error)
}

var _ ActivationStore = (*store)(nil)

func (s *store) CurrentActivation(ctx context.Context, sessionID int64) (*ToolActivation, error) {
	activation, err := scanActivation(s.db.QueryRowContext(ctx, `SELECT input_id, session_id, tool_id,
		command, state, COALESCE(tool_call_id, ''), created_at, resolved_at
		FROM session_tool_activations WHERE session_id = ? AND state IN ('pending', 'consumed')
		ORDER BY CASE state WHEN 'pending' THEN 0 ELSE 1 END, input_id DESC LIMIT 1`, sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrActivationNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("load current activation: %w", err)
	}

	return activation, nil
}

func (s *store) PendingActivation(ctx context.Context, sessionID int64) (*ToolActivation, error) {
	activation, err := scanActivation(s.db.QueryRowContext(ctx, `SELECT input_id, session_id, tool_id,
		command, state, COALESCE(tool_call_id, ''), created_at, resolved_at
		FROM session_tool_activations WHERE session_id = ? AND state = 'pending'`, sessionID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrActivationNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("load pending activation: %w", err)
	}

	return activation, nil
}

func (s *store) ConsumeActivation(
	ctx context.Context,
	inputID, sessionID int64,
	toolID, command, toolCallID string,
) (*ToolActivation, error) {
	if inputID <= 0 || sessionID <= 0 || toolID == "" || command == "" || toolCallID == "" {
		return nil, ErrActivationConflict
	}

	now := time.Now().UTC()

	result, err := s.db.ExecContext(ctx, `UPDATE session_tool_activations
		SET state = 'consumed', tool_call_id = ?, resolved_at = ?
		WHERE input_id = ? AND session_id = ? AND tool_id = ? AND command = ? AND state = 'pending'`,
		toolCallID, now, inputID, sessionID, toolID, command)
	if err != nil {
		return nil, fmt.Errorf("consume tool activation: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("consume tool activation rows affected: %w", err)
	}

	if affected == 0 {
		existing, loadErr := s.activationByInput(ctx, inputID)
		if loadErr == nil && existing.SessionID == sessionID && existing.ToolID == toolID &&
			existing.Command == command && existing.ToolCallID == toolCallID && existing.State == ActivationConsumed {
			return existing, nil
		}

		return nil, ErrActivationConflict
	}

	return s.activationByInput(ctx, inputID)
}

func (s *store) ExpireActivation(
	ctx context.Context,
	inputID, sessionID int64,
) (*ToolActivation, error) {
	result, err := s.db.ExecContext(ctx, `UPDATE session_tool_activations
		SET state = 'expired', resolved_at = ?
		WHERE input_id = ? AND session_id = ? AND state = 'pending'`,
		time.Now().UTC(), inputID, sessionID)
	if err != nil {
		return nil, fmt.Errorf("expire tool activation: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("expire tool activation rows affected: %w", err)
	}

	if affected == 0 {
		existing, loadErr := s.activationByInput(ctx, inputID)
		if loadErr == nil && existing.SessionID == sessionID && existing.State == ActivationExpired {
			return existing, nil
		}

		return nil, ErrActivationConflict
	}

	return s.activationByInput(ctx, inputID)
}

func (s *store) ExpireActivationWithOutput(
	ctx context.Context,
	inputID, sessionID int64,
	content string,
) (*ToolActivation, *OutputCommit, error) {
	if content == "" {
		return nil, nil, ErrActivationConflict
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin activation expiry: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	owner, err := outputOwner(ctx, tx, sessionID)
	if err != nil {
		return nil, nil, err
	}

	now := time.Now().UTC()

	result, err := tx.ExecContext(ctx, `UPDATE session_tool_activations
		SET state = 'expired', resolved_at = ?
		WHERE input_id = ? AND session_id = ? AND state = 'pending'`, now, inputID, sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("expire activation with output: %w", err)
	}

	affected, _ := result.RowsAffected()
	if affected == 0 {
		activation, loadErr := scanActivation(tx.QueryRowContext(ctx, `SELECT input_id, session_id,
			tool_id, command, state, COALESCE(tool_call_id, ''), created_at, resolved_at
			FROM session_tool_activations WHERE input_id = ?`, inputID))
		if loadErr != nil || activation.State != ActivationExpired {
			return nil, nil, ErrActivationConflict
		}
	}

	commit, err := insertMessageOutput(ctx, tx, sessionID, owner, content,
		fmt.Sprintf("input:%d:activation:expired", inputID), now, false)
	if err != nil {
		if !isUniqueConstraintError(err) {
			return nil, nil, err
		}

		var id int64
		if err := tx.QueryRowContext(ctx, `SELECT id FROM session_outbox
			WHERE session_id = ? AND source_key = ?`, sessionID,
			fmt.Sprintf("input:%d:activation:expired", inputID)).Scan(&id); err != nil {
			return nil, nil, err
		}

		commit = &OutputCommit{OutputID: id, OwnerID: owner, Existing: true}
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit activation expiry: %w", err)
	}

	activation, err := s.activationByInput(ctx, inputID)

	return activation, commit, err
}

func (s *store) activationByInput(ctx context.Context, inputID int64) (*ToolActivation, error) {
	activation, err := scanActivation(s.db.QueryRowContext(ctx, `SELECT input_id, session_id, tool_id,
		command, state, COALESCE(tool_call_id, ''), created_at, resolved_at
		FROM session_tool_activations WHERE input_id = ?`, inputID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrActivationNotFound
	}

	if err != nil {
		return nil, fmt.Errorf("load tool activation: %w", err)
	}

	return activation, nil
}

func scanActivation(row interface{ Scan(...any) error }) (*ToolActivation, error) {
	var activation ToolActivation

	var resolved sql.NullTime
	if err := row.Scan(&activation.InputID, &activation.SessionID, &activation.ToolID,
		&activation.Command, &activation.State, &activation.ToolCallID, &activation.CreatedAt, &resolved); err != nil {
		return nil, err
	}

	if resolved.Valid {
		activation.ResolvedAt = &resolved.Time
	}

	return &activation, nil
}

func requireActivationChanged(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("tool activation rows affected: %w", err)
	}

	if affected != 1 {
		return ErrActivationConflict
	}

	return nil
}

// TryFinalizeSubagentActivation linearizes terminalization against durable
// follow-up acceptance and /stop.
//
//nolint:goconst // Link-state strings are a separate durable vocabulary from session status.
func (s *store) TryFinalizeSubagentActivation(
	ctx context.Context,
	childID int64,
	state, result, outcome string,
) (bool, error) {
	if state != "completed" && state != "error" {
		return false, fmt.Errorf("invalid activation terminal state %q", state)
	}

	if outcome != "completed" && outcome != "error" && outcome != "incomplete" {
		return false, fmt.Errorf("invalid activation outcome %q", outcome)
	}

	execResult, err := s.db.ExecContext(ctx, `UPDATE subagent_links
		SET state = ?, result = ?, outcome = ?
		WHERE child_id = ? AND state IN ('spawned', 'running')
			AND EXISTS (SELECT 1 FROM sessions sess
				WHERE sess.id = subagent_links.child_id
					AND sess.status NOT IN ('stopping', 'stopped', 'killed')
					AND sess.killed_at IS NULL)
			AND NOT EXISTS (SELECT 1 FROM session_inbox input
				WHERE input.session_id = subagent_links.child_id AND input.state = 'pending')`,
		state, result, outcome, childID)
	if err != nil {
		return false, fmt.Errorf("conditionally finalize subagent activation: %w", err)
	}

	rows, err := execResult.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("finalize subagent activation rows affected: %w", err)
	}

	return rows == 1, nil
}

// RearmDeliveredSubagentWithPendingInput starts the next activation only after
// the prior outcome is durable in the parent transcript.
func (s *store) RearmDeliveredSubagentWithPendingInput(ctx context.Context, childID int64) (bool, error) {
	execResult, err := s.db.ExecContext(ctx, `UPDATE subagent_links
		SET state = 'running', blocking = 0, activation_seq = activation_seq + 1,
			delivered_at = NULL, delivered_msg_id = NULL
		WHERE child_id = ? AND state IN ('completed', 'error') AND delivered_at IS NOT NULL
			AND EXISTS (SELECT 1 FROM session_inbox input
				WHERE input.session_id = subagent_links.child_id AND input.state = 'pending')`, childID)
	if err != nil {
		return false, fmt.Errorf("rearm delivered subagent activation: %w", err)
	}

	rows, err := execResult.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rearm subagent activation rows affected: %w", err)
	}

	return rows == 1, nil
}
