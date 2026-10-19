package assistant

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

const (
	userKind    = "user"
	sayKind     = "say"
	observeKind = "observe"
	moveKind    = "move"
	errorKind   = "error"
	// conversationLines bounds the dialog the desk repeats; older lines are found by search.
	conversationLines = 10
)

var _ Store = (*store)(nil)

// Searcher finds what fell off the desk; the FTS store is one backend.
type Searcher interface {
	Search(context.Context, Scope, string) ([]Hit, error)
}

// Store reads the assistant's durable memory, history and the work it may address.
type Store interface {
	Searcher
	LoadDesk(context.Context, int64, int64, time.Time) (Desk, error)
	Memory(context.Context, int64) ([]Memory, error)
	Focus(context.Context, int64) ([]Focus, error)
	Around(context.Context, Scope, string, int) (string, error)
	WorkRoots(context.Context, Scope, bool) ([]WorkRoot, error)
	Authorized(context.Context, string, int64) (bool, error)
	RecentTape(context.Context, int64, int) ([]Line, error)
	UnobservedSends(context.Context, int64) ([]Send, error)
}

// Desk is the durable part of what one turn sees; expired memory is already left out.
type Desk struct {
	Memory       []Memory
	Focus        []Focus
	Conversation []Line
	Spoken       int
}

// Memory is permanent when Until is empty; otherwise it is live through that UTC date.
type Memory struct {
	ID        int64
	Text      string
	Until     string
	UpdatedAt time.Time
}

// Focus refs are session ids or this turn's open handles.
type Focus struct {
	ID        int64
	Text      string
	Sessions  []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Line retains its identity after it falls off the desk.
type Line struct {
	ID   int64
	At   time.Time
	Kind string
	Text string
}

// Send links an assistant instruction to the work session's durable input.
type Send struct {
	InputID            int64
	AssistantSessionID int64
	SessionID          int64
	CreatedAt          time.Time
}

// Call records one model call of a turn for inspection only.
type Call struct {
	SessionID int64
	InputID   int64
	Number    int
	Phase     string
	Request   string
	Response  string
	Outcome   string
	CreatedAt time.Time
}

// Scope names the assistant root and the manager whose work roots it may address.
type Scope struct {
	Root  int64
	Owner string
}

type store struct {
	db *sql.DB
}

// New shares the session database without owning its lifetime.
func New(db *sql.DB) Store {
	return &store{db: db}
}

// Live reports whether the note still belongs on the desk on the given UTC date.
func (m Memory) Live(today string) bool {
	return m.Until == "" || m.Until >= today
}

// LoadDesk shows the dialog before line `before`, so the turn's own event is not repeated.
func (s *store) LoadDesk(ctx context.Context, root, before int64, now time.Time) (Desk, error) {
	memory, err := s.Memory(ctx, root)
	if err != nil {
		return Desk{}, err
	}

	desk := Desk{}
	today := now.UTC().Format(time.DateOnly)

	for _, note := range memory {
		if note.Live(today) {
			desk.Memory = append(desk.Memory, note)
		}
	}

	desk.Focus, err = s.Focus(ctx, root)
	if err != nil {
		return Desk{}, err
	}

	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM assistant_tape
		WHERE session_id = ? AND kind IN ('user', 'say') AND (? = 0 OR id < ?)`, root, before, before).
		Scan(&desk.Spoken); err != nil {
		return Desk{}, fmt.Errorf("count assistant conversation: %w", err)
	}

	desk.Conversation, err = s.tape(ctx, `SELECT id, at, kind, text FROM (
		SELECT id, at, kind, text FROM assistant_tape
		WHERE session_id = ? AND kind IN ('user', 'say') AND (? = 0 OR id < ?) ORDER BY id DESC LIMIT ?
	) ORDER BY id`, root, before, before, conversationLines)
	if err != nil {
		return Desk{}, fmt.Errorf("load assistant conversation: %w", err)
	}

	return desk, nil
}

// Memory includes expired notes: search can still surface them for rewriting or removal.
func (s *store) Memory(ctx context.Context, root int64) ([]Memory, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, text, COALESCE(until, ''), updated_at
		FROM assistant_memory WHERE session_id = ? ORDER BY id`, root)
	if err != nil {
		return nil, fmt.Errorf("load assistant memory: %w", err)
	}
	defer rows.Close()

	var notes []Memory

	for rows.Next() {
		var note Memory
		if err := rows.Scan(&note.ID, &note.Text, &note.Until, &note.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan assistant memory: %w", err)
		}

		notes = append(notes, note)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read assistant memory: %w", err)
	}

	return notes, nil
}

// Focus returns every live item, most recently updated first.
func (s *store) Focus(ctx context.Context, root int64) ([]Focus, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, text, sessions, created_at, updated_at
		FROM assistant_focus WHERE session_id = ?`, root)
	if err != nil {
		return nil, fmt.Errorf("load assistant focus: %w", err)
	}
	defer rows.Close()

	var items []Focus

	for rows.Next() {
		var item Focus
		var refs string

		if err := rows.Scan(&item.ID, &item.Text, &refs, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan assistant focus: %w", err)
		}

		if err := json.Unmarshal([]byte(refs), &item.Sessions); err != nil {
			return nil, fmt.Errorf("decode assistant focus %d sessions: %w", item.ID, err)
		}

		items = append(items, item)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read assistant focus: %w", err)
	}

	slices.SortStableFunc(items, func(a, b Focus) int {
		if order := b.UpdatedAt.Compare(a.UpdatedAt); order != 0 {
			return order
		}

		return cmp.Compare(b.ID, a.ID)
	})

	return items, nil
}

// RecentTape returns the newest lines, hidden ones included, oldest first.
func (s *store) RecentTape(ctx context.Context, root int64, limit int) ([]Line, error) {
	lines, err := s.tape(ctx, `SELECT id, at, kind, text FROM (
		SELECT id, at, kind, text FROM assistant_tape WHERE session_id = ? ORDER BY id DESC LIMIT ?
	) ORDER BY id`, root, max(0, limit))
	if err != nil {
		return nil, fmt.Errorf("load recent assistant tape: %w", err)
	}

	return lines, nil
}

// AppendTapeTx never reuses a committed line's number; dialog lines become searchable with it.
func AppendTapeTx(ctx context.Context, tx *sql.Tx, root int64, line Line) (int64, error) {
	result, err := tx.ExecContext(ctx, `INSERT INTO assistant_tape (session_id, at, kind, text)
		VALUES (?, ?, ?, ?)`, root, line.At, line.Kind, line.Text)
	if err != nil {
		return 0, fmt.Errorf("append assistant tape: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("assistant tape id: %w", err)
	}

	if line.Kind == userKind || line.Kind == sayKind {
		if err := indexTx(ctx, tx, searchLine, id, root, line.Text); err != nil {
			return 0, err
		}
	}

	return id, nil
}

// RecordCallTx keeps every call of committed and failed turns for inspection.
func RecordCallTx(ctx context.Context, tx *sql.Tx, call Call) error {
	_, err := tx.ExecContext(
		ctx,
		`INSERT INTO assistant_turns
		(session_id, input_id, call, phase, request, response, outcome, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		call.SessionID,
		call.InputID,
		call.Number,
		call.Phase,
		call.Request,
		call.Response,
		call.Outcome,
		call.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("record assistant call: %w", err)
	}

	return nil
}

func (s *store) tape(ctx context.Context, query string, args ...any) ([]Line, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query assistant tape: %w", err)
	}
	defer rows.Close()

	var lines []Line

	for rows.Next() {
		var line Line
		if err := rows.Scan(&line.ID, &line.At, &line.Kind, &line.Text); err != nil {
			return nil, fmt.Errorf("scan assistant tape: %w", err)
		}

		lines = append(lines, line)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read assistant tape: %w", err)
	}

	return lines, nil
}
