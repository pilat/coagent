package backgroundprocess

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const processColumns = `id, session_id, root_session_id, tool_call_id, output_path,
	deadline_at, created_at, advertised_at, output_size, exit_code, host_intent,
	state, finished_at`

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
	ListRunningBySessions(ctx context.Context, sessionIDs []int64) ([]Process, error)
	RecordIntent(ctx context.Context, id string, intent HostIntent) (bool, error)
	Advertise(ctx context.Context, id string, at time.Time) (bool, error)
	Finalize(ctx context.Context, id string, natural State, exitCode *int, outputSize int64) (Process, bool, error)
	FinalizeWithIntent(
		ctx context.Context,
		id string,
		intent HostIntent,
		outputSize int64,
	) (Process, bool, error)
	UpdateOutputSize(ctx context.Context, id string, size int64) error
	CountTerminalByIntentSince(
		ctx context.Context,
		rootSessionID int64,
		intent HostIntent,
		since time.Time,
	) (int, error)
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
			deadline_at, created_at, advertised_at, output_size, host_intent, state
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
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

func (s *store) ListRunningBySessions(ctx context.Context, sessionIDs []int64) ([]Process, error) {
	encoded, err := json.Marshal(sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("encode process owner sessions: %w", err)
	}

	return s.list(ctx,
		`SELECT `+processColumns+` FROM background_processes
		 WHERE session_id IN (SELECT value FROM json_each(?)) AND state = 'running'`, encoded,
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

// Advertise promotes a still-running unadvertised candidate at the
// foreground-grace boundary. won=false means the process already
// terminalized; the foreground result then wins over background promotion.
func (s *store) Advertise(ctx context.Context, id string, at time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE background_processes SET advertised_at = ?
		WHERE id = ? AND advertised_at IS NULL AND state = 'running'`,
		at, id,
	)
	if err != nil {
		return false, fmt.Errorf("advertise process: %w", err)
	}

	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("advertise process: %w", err)
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
	outputSize int64,
) (Process, bool, error) {
	return s.finalize(ctx, id, natural, exitCode, outputSize, IntentNone)
}

func (s *store) FinalizeWithIntent(
	ctx context.Context,
	id string,
	intent HostIntent,
	outputSize int64,
) (Process, bool, error) {
	if intent == IntentNone {
		return Process{}, false, errors.New("finalize with intent: empty intent")
	}

	return s.finalize(ctx, id, IntentToState(intent), nil, outputSize, intent)
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

func (s *store) CountTerminalByIntentSince(
	ctx context.Context,
	rootSessionID int64,
	intent HostIntent,
	since time.Time,
) (int, error) {
	var count int

	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM background_processes
		WHERE root_session_id = ? AND host_intent = ? AND state <> 'running' AND finished_at >= ?`,
		rootSessionID, string(intent), since).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count terminal processes by intent: %w", err)
	}

	return count, nil
}

func (s *store) finalize(
	ctx context.Context,
	id string,
	natural State,
	exitCode *int,
	outputSize int64,
	fallbackIntent HostIntent,
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
		winner, err := scanProcess(tx.QueryRowContext(ctx,
			`SELECT `+processColumns+` FROM background_processes WHERE id = ?`, id,
		))
		if err != nil {
			return Process{}, false, err
		}

		return winner, false, nil
	}

	outcome := natural
	persistedFallback := IntentNone

	if HostIntent(intent) != IntentNone {
		outcome = IntentToState(HostIntent(intent))
	} else if fallbackIntent != IntentNone {
		outcome = IntentToState(fallbackIntent)
		persistedFallback = fallbackIntent
	}

	winner, won, err := s.finalizeRunning(
		ctx, tx, id, outcome, exitCode, outputSize, persistedFallback,
	)
	if err != nil {
		return Process{}, false, err
	}

	if !won {
		_ = tx.Rollback()

		latest, err := s.GetProcess(ctx, id)
		if err != nil {
			return Process{}, false, err
		}

		return latest, false, nil
	}

	if err := tx.Commit(); err != nil {
		return Process{}, false, fmt.Errorf("finalize process: %w", err)
	}

	return winner, true, nil
}
