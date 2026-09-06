package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	ShieldRaiseProgressContent   = "🛡️ Raising shields…"
	ShieldRaisedContent          = "🛡️ Shields raised."
	ShieldLoweredContent         = "🛡️ Shields lowered."
	ShieldLowerBusyContent       = "Cannot lower shields while the session tree is running; retry when it is idle."
	ShieldSandboxDisabledContent = "Cannot raise shields while sandbox.enabled is false."
)

const (
	shieldRaiseReason = "shieldsup"
	shieldDownReason  = "shieldsdown"
)

var ErrInvalidShieldCommand = errors.New("invalid session shields command")

// ShieldRaise records what the daemon must finish after the begin transaction.
type ShieldRaise struct {
	RootID    int64
	InputID   int64
	Changed   bool
	NeedsStop bool
}

// InterruptedShieldRaise is a handled raise whose terminal output is still owed.
type InterruptedShieldRaise struct {
	SessionID int64
	InputID   int64
}

// ShieldCommandStore owns the durable two-phase raise and atomic lowering commands.
type ShieldCommandStore interface {
	BeginShieldRaise(
		ctx context.Context,
		inputID int64,
		treeActive bool,
		sandboxEnabled bool,
	) (*ShieldRaise, *OutputCommit, error)
	CompleteShieldRaise(ctx context.Context, rootID, inputID int64) (*OutputCommit, error)
	ResolveShieldDown(ctx context.Context, inputID int64, treeActive bool) (*OutputCommit, error)
	SelectInterruptedShieldRaises(ctx context.Context) ([]InterruptedShieldRaise, error)
	CancelPendingInputsPreservingShieldCommands(
		ctx context.Context,
		sessionIDs []int64,
		reason string,
	) (int64, error)
}

var _ ShieldCommandStore = (*store)(nil)

//nolint:wsl_v5 // Replay detection and first-phase persistence share one transaction.
func (s *store) BeginShieldRaise(
	ctx context.Context,
	inputID int64,
	treeActive bool,
	sandboxEnabled bool,
) (*ShieldRaise, *OutputCommit, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin shield raise: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	input, owner, replay, err := loadShieldInput(ctx, tx, inputID, "/shieldsup", shieldRaiseReason)
	if err != nil {
		return nil, nil, err
	}
	if replay {
		return replayShieldRaise(ctx, tx, input, owner)
	}

	var shieldsUp bool
	if err := tx.QueryRowContext(ctx, `SELECT shields_up FROM sessions WHERE id = ?`, input.SessionID).
		Scan(&shieldsUp); err != nil {
		return nil, nil, fmt.Errorf("load shield raise state: %w", err)
	}

	now := time.Now().UTC()
	if !sandboxEnabled {
		return finishImmediateShieldRaise(
			ctx, tx, input, owner, "shieldsup:unavailable", ShieldSandboxDisabledContent,
		)
	}

	if shieldsUp {
		return finishImmediateShieldRaise(
			ctx, tx, input, owner, "shieldsup:completed", ShieldRaisedContent,
		)
	}

	return beginChangedShieldRaise(ctx, tx, input, owner, treeActive, now)
}

//nolint:wsl_v5 // Active and idle raises preserve one ordered transaction protocol.
func beginChangedShieldRaise(
	ctx context.Context,
	tx *sql.Tx,
	input *InboxInput,
	owner string,
	treeActive bool,
	now time.Time,
) (*ShieldRaise, *OutputCommit, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET shields_up = TRUE, updated_at = ?
		WHERE id = ? OR root_id = ?`, now, input.SessionID, input.SessionID); err != nil {
		return nil, nil, fmt.Errorf("raise session tree shields: %w", err)
	}
	if treeActive {
		result, err := tx.ExecContext(ctx, `UPDATE sessions SET status = 'stopping', updated_at = ?
			WHERE id = ? AND killed_at IS NULL
				AND status IN ('active', 'completed', 'suspended', 'error', 'stopped')`, now, input.SessionID)
		if err != nil {
			return nil, nil, fmt.Errorf("fence active shield raise: %w", err)
		}
		if err := requireOneSessionUpdate(result, input.SessionID); err != nil {
			return nil, nil, err
		}
	}
	if err := handleShieldInput(ctx, tx, input.ID, shieldRaiseReason, now); err != nil {
		return nil, nil, err
	}
	commit, err := insertShieldOutput(
		ctx, tx, input.SessionID, input.ID, owner, "shieldsup:started",
		OutputMessageReplaceable, ShieldRaiseProgressContent, false, now,
	)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit shield raise start: %w", err)
	}

	return &ShieldRaise{
		RootID: input.SessionID, InputID: input.ID, Changed: true, NeedsStop: treeActive,
	}, commit, nil
}

//nolint:wsl_v5 // Immediate output and commit are one durable transition.
func finishImmediateShieldRaise(
	ctx context.Context,
	tx *sql.Tx,
	input *InboxInput,
	owner, suffix, content string,
) (*ShieldRaise, *OutputCommit, error) {
	now := time.Now().UTC()
	if err := handleShieldInput(ctx, tx, input.ID, shieldRaiseReason, now); err != nil {
		return nil, nil, err
	}
	commit, err := insertShieldOutput(
		ctx, tx, input.SessionID, input.ID, owner, suffix,
		OutputMessagePersistent, content, true, now,
	)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit immediate shield raise: %w", err)
	}

	return &ShieldRaise{RootID: input.SessionID, InputID: input.ID}, commit, nil
}

//nolint:wsl_v5 // Recovery and first execution converge on one idempotent completion.
func (s *store) CompleteShieldRaise(
	ctx context.Context,
	rootID, inputID int64,
) (*OutputCommit, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin shield raise completion: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	owner, err := outputOwner(ctx, tx, rootID)
	if err != nil {
		return nil, err
	}

	var status SessionStatus
	var started bool
	if err := tx.QueryRowContext(ctx, `SELECT status, EXISTS (
		SELECT 1 FROM session_outbox WHERE session_id = ?
			AND source_key = 'input:' || ? || ':shieldsup:started')
		FROM sessions WHERE id = ? AND shields_up = TRUE`, rootID, inputID, rootID).
		Scan(&status, &started); err != nil {
		return nil, fmt.Errorf("load shield raise completion: %w", err)
	}
	if !started {
		return nil, fmt.Errorf("%w: shield raise %d has no start", ErrInvalidShieldCommand, inputID)
	}

	now := time.Now().UTC()
	if status == SessionStatusStopping {
		if _, err := tx.ExecContext(ctx, `UPDATE session_budgets
			SET state = 'released', released_at = ?, released_reason = 'stopped', park_owner = ''
			WHERE root_session_id = ? AND state = 'armed'`, now, rootID); err != nil {
			return nil, fmt.Errorf("release armed budget for shield raise: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET status = 'stopped', updated_at = ?
			WHERE id = ? AND status = 'stopping'`, now, rootID); err != nil {
			return nil, fmt.Errorf("park raised root: %w", err)
		}
	}

	commit, err := insertShieldOutput(
		ctx, tx, rootID, inputID, owner, "shieldsup:completed",
		OutputMessagePersistent, ShieldRaisedContent, true, now,
	)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit shield raise completion: %w", err)
	}

	return commit, nil
}

//nolint:wsl_v5 // Validation, busy detection, and output persistence share one transaction.
func (s *store) ResolveShieldDown(
	ctx context.Context,
	inputID int64,
	treeActive bool,
) (*OutputCommit, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin shield lowering: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	input, owner, replay, err := loadShieldInput(ctx, tx, inputID, "/shieldsdown", shieldDownReason)
	if err != nil {
		return nil, err
	}
	if replay {
		return replayShieldDown(ctx, tx, input, owner)
	}

	var shieldsUp bool
	if err := tx.QueryRowContext(ctx, `SELECT shields_up FROM sessions WHERE id = ?`, input.SessionID).
		Scan(&shieldsUp); err != nil {
		return nil, fmt.Errorf("load shield lowering state: %w", err)
	}

	content := ShieldLoweredContent
	suffix := "shieldsdown:completed"
	if shieldsUp && treeActive {
		content = ShieldLowerBusyContent
		suffix = "shieldsdown:busy"
	} else if shieldsUp {
		if _, err := tx.ExecContext(ctx, `UPDATE sessions SET shields_up = FALSE, updated_at = ?
			WHERE id = ? OR root_id = ?`, time.Now().UTC(), input.SessionID, input.SessionID); err != nil {
			return nil, fmt.Errorf("lower session tree shields: %w", err)
		}
	}

	now := time.Now().UTC()
	if err := handleShieldInput(ctx, tx, input.ID, shieldDownReason, now); err != nil {
		return nil, err
	}
	commit, err := insertShieldOutput(
		ctx, tx, input.SessionID, input.ID, owner, suffix,
		OutputMessagePersistent, content, true, now,
	)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit shield lowering: %w", err)
	}

	return commit, nil
}
