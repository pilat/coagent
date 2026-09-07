package backgroundprocess

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const processColumns = `id, session_id, root_session_id, tool_call_id, output_path,
	deadline_at, created_at, advertised_at, output_size, exit_code, host_intent,
	state, finished_at, delivery_state, delivery_target_session_id, delivered_at`

var _ Store = (*store)(nil)

type store struct {
	db *sql.DB
}

// Store owns ordinary durable background-process ledger access.
type Store interface {
	InsertProcess(ctx context.Context, process Process) error
	GetProcess(ctx context.Context, id string) (Process, error)
	ListRunning(ctx context.Context) ([]Process, error)
	ListRunningAdvertised(ctx context.Context) ([]Process, error)
	ListRunningByRoot(ctx context.Context, rootSessionID int64) ([]Process, error)
	RecordIntent(ctx context.Context, id string, intent HostIntent) (bool, error)
	Finalize(ctx context.Context, id string, natural State, exitCode *int) (Process, bool, error)
	ClaimDelivery(ctx context.Context, id string, targetSessionID int64) (bool, error)
	MarkDelivered(ctx context.Context, id string) (bool, error)
	MarkSuppressed(ctx context.Context, id string) (bool, error)
	UpdateOutputSize(ctx context.Context, id string, size int64) error
	ListUndelivered(ctx context.Context) ([]Process, error)
}

// NewStore returns the SQL-backed process ledger.
func NewStore(db *sql.DB) Store {
	return &store{db: db}
}

func (s *store) InsertProcess(ctx context.Context, process Process) error {
	var advertisedAt any
	if process.AdvertisedAt != nil {
		advertisedAt = *process.AdvertisedAt
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO background_processes (
			id, session_id, root_session_id, tool_call_id, output_path,
			deadline_at, created_at, advertised_at, output_size, host_intent, state,
			delivery_state
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')`,
		process.ID,
		process.SessionID,
		process.RootSessionID,
		process.ToolCallID,
		process.OutputPath,
		process.Deadline,
		process.CreatedAt,
		advertisedAt,
		process.OutputSize,
		string(process.HostIntent),
		string(process.State),
	)
	if err != nil {
		return fmt.Errorf("insert background process: %w", err)
	}

	return nil
}

func (s *store) GetProcess(ctx context.Context, id string) (Process, error) {
	row := s.db.QueryRowContext(
		ctx, `SELECT `+processColumns+` FROM background_processes WHERE id = ?`, id,
	)

	return scanProcess(row)
}

func (s *store) ListRunning(ctx context.Context) ([]Process, error) {
	return s.list(ctx,
		`SELECT `+processColumns+` FROM background_processes WHERE state = 'running'`,
	)
}

func (s *store) ListRunningAdvertised(ctx context.Context) ([]Process, error) {
	return s.list(ctx,
		`SELECT `+processColumns+` FROM background_processes
		 WHERE state = 'running' AND advertised_at IS NOT NULL`,
	)
}

func (s *store) ListRunningByRoot(ctx context.Context, rootSessionID int64) ([]Process, error) {
	return s.list(ctx,
		`SELECT `+processColumns+` FROM background_processes
		 WHERE root_session_id = ? AND state = 'running'`, rootSessionID,
	)
}

// RecordIntent durably records the first terminal cause. It fails only when
// the process already left the running state; an existing intent is kept.
func (s *store) RecordIntent(ctx context.Context, id string, intent HostIntent) (bool, error) {
	if intent == IntentNone {
		return false, errors.New("record intent: empty intent")
	}

	res, err := s.db.ExecContext(ctx, `
		UPDATE background_processes SET host_intent = ?
		WHERE id = ? AND state = 'running' AND host_intent = ''`,
		string(intent), id,
	)
	if err != nil {
		return false, fmt.Errorf("record process intent: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record process intent: %w", err)
	}

	return rows == 1, nil
}

// Finalize terminalizes the process with first-intent precedence: a recorded
// host intent decides the outcome, natural exit wins only without one. The
// losing caller re-reads the winning row. Returns won=false when already
// terminal.
func (s *store) Finalize(
	ctx context.Context,
	id string,
	natural State,
	exitCode *int,
) (Process, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Process{}, false, fmt.Errorf("finalize process: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	var intent string
	var state string

	err = tx.QueryRowContext(ctx,
		`SELECT host_intent, state FROM background_processes WHERE id = ?`, id,
	).Scan(&intent, &state)
	if err != nil {
		return Process{}, false, fmt.Errorf("finalize process: %w", err)
	}

	if state != string(StateRunning) {
		winner, err := s.GetProcess(ctx, id)
		if err != nil {
			return Process{}, false, err
		}

		return winner, false, nil
	}

	outcome := natural
	if HostIntent(intent) != IntentNone {
		outcome = IntentToState(HostIntent(intent))
	}

	now := time.Now().UTC()

	res, err := tx.ExecContext(ctx, `
		UPDATE background_processes
		SET state = ?, exit_code = COALESCE(?, exit_code), finished_at = ?
		WHERE id = ? AND state = 'running'`,
		string(outcome), exitCode, now, id,
	)
	if err != nil {
		return Process{}, false, fmt.Errorf("finalize process: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return Process{}, false, fmt.Errorf("finalize process: %w", err)
	}

	if rows != 1 {
		winner, err := s.GetProcess(ctx, id)
		if err != nil {
			return Process{}, false, err
		}

		return winner, false, nil
	}

	winner, err := scanProcess(tx.QueryRowContext(ctx,
		`SELECT `+processColumns+` FROM background_processes WHERE id = ?`, id,
	))
	if err != nil {
		return Process{}, false, fmt.Errorf("finalize process: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return Process{}, false, fmt.Errorf("finalize process: %w", err)
	}

	return winner, true, nil
}

func (s *store) ClaimDelivery(ctx context.Context, id string, targetSessionID int64) (bool, error) {
	return s.claim(ctx, id, targetSessionID, false)
}

func (s *store) MarkDelivered(ctx context.Context, id string) (bool, error) {
	return s.completeDelivery(ctx, id, "delivered")
}

func (s *store) MarkSuppressed(ctx context.Context, id string) (bool, error) {
	return s.completeDelivery(ctx, id, "suppressed")
}

func (s *store) UpdateOutputSize(ctx context.Context, id string, size int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE background_processes SET output_size = ? WHERE id = ?`, size, id,
	)
	if err != nil {
		return fmt.Errorf("update process output size: %w", err)
	}

	return nil
}

// ListUndelivered returns terminal records whose completion was claimed but
// never confirmed delivered, plus terminal records still pending. Startup
// recovery re-routes them exactly once.
func (s *store) ListUndelivered(ctx context.Context) ([]Process, error) {
	return s.list(ctx,
		`SELECT `+processColumns+` FROM background_processes
		 WHERE state <> 'running'
		   AND delivery_state IN ('pending', 'claimed')
		   AND advertised_at IS NOT NULL`,
	)
}

func (s *store) claim(
	ctx context.Context,
	id string,
	targetSessionID int64,
	_ bool,
) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE background_processes
		SET delivery_state = 'claimed', delivery_target_session_id = ?
		WHERE id = ? AND delivery_state = 'pending'`,
		targetSessionID, id,
	)
	if err != nil {
		return false, fmt.Errorf("claim process delivery: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim process delivery: %w", err)
	}

	return rows == 1, nil
}

func (s *store) completeDelivery(ctx context.Context, id, deliveryState string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE background_processes
		SET delivery_state = ?, delivered_at = ?
		WHERE id = ? AND delivery_state = 'claimed'`,
		deliveryState, time.Now().UTC(), id,
	)
	if err != nil {
		return false, fmt.Errorf("complete process delivery: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("complete process delivery: %w", err)
	}

	return rows == 1, nil
}

func (s *store) list(ctx context.Context, query string, args ...any) ([]Process, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list background processes: %w", err)
	}

	defer rows.Close()

	var processes []Process

	for rows.Next() {
		process, err := scanProcess(rows)
		if err != nil {
			return nil, err
		}

		processes = append(processes, process)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list background processes: %w", err)
	}

	return processes, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanProcess(row rowScanner) (Process, error) {
	var process Process
	var hostIntent string
	var state string
	var deliveryState string
	var targetSessionID sql.NullInt64

	err := row.Scan(
		&process.ID,
		&process.SessionID,
		&process.RootSessionID,
		&process.ToolCallID,
		&process.OutputPath,
		&process.Deadline,
		&process.CreatedAt,
		&process.AdvertisedAt,
		&process.OutputSize,
		&process.ExitCode,
		&hostIntent,
		&state,
		&process.FinishedAt,
		&deliveryState,
		&targetSessionID,
		&process.DeliveredAt,
	)
	if err != nil {
		return Process{}, fmt.Errorf("scan background process row: %w", err)
	}

	if targetSessionID.Valid {
		process.DeliveryTargetSessionID = targetSessionID.Int64
	}

	process.HostIntent = HostIntent(hostIntent)
	process.State = State(state)
	process.DeliveryState = deliveryState

	return process, nil
}
