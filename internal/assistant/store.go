package assistant

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

var _ Store = (*store)(nil)

// Store reads the assistant's durable memory and outstanding sends.
type Store interface {
	LoadDesk(context.Context, int64) (Desk, error)
	VisibleTape(context.Context, int64, int, int64) ([]Line, error)
	RecentTape(context.Context, int64, int) ([]Line, error)
	UnobservedSends(context.Context, int64) ([]Send, error)
}

// Desk is saved whole after each successful step has applied its edits.
type Desk struct {
	Diary      map[string]string
	Stickers   []Sticker
	Focus      string
	LastTurnAt time.Time
	UpdatedAt  time.Time
	TapeLastID int64
}

// Sticker is a keyed short-term note. On is the session id it waits on, or the
// project name of an open until that open lands.
type Sticker struct {
	Key  string `json:"key"`
	Text string `json:"text"`
	On   string `json:"on,omitempty"`
}

// Line retains its identity even after it is hidden from the desk.
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

// Turn records one model attempt, including referee retries.
type Turn struct {
	SessionID int64
	InputID   int64
	Step      int
	Desk      string
	Response  string
	Outcome   string
	CreatedAt time.Time
}

type store struct {
	db *sql.DB
}

// New shares the session database without owning its lifetime.
func New(db *sql.DB) Store {
	return &store{db: db}
}

// LoadDesk returns empty slots before the first successful step.
func (s *store) LoadDesk(ctx context.Context, sessionID int64) (Desk, error) {
	desk := Desk{Diary: map[string]string{}, Stickers: []Sticker{}}
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM assistant_tape WHERE session_id = ?`,
		sessionID).Scan(&desk.TapeLastID); err != nil {
		return Desk{}, fmt.Errorf("load assistant tape position: %w", err)
	}

	var diary, stickers string
	var last sql.NullTime

	err := s.db.QueryRowContext(ctx, `SELECT diary, stickers, focus, last_turn_at, updated_at
		FROM assistant_desk WHERE session_id = ?`, sessionID).
		Scan(&diary, &stickers, &desk.Focus, &last, &desk.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return desk, nil
	}

	if err != nil {
		return Desk{}, fmt.Errorf("load assistant desk: %w", err)
	}

	if err := json.Unmarshal([]byte(diary), &desk.Diary); err != nil {
		return Desk{}, fmt.Errorf("decode assistant diary: %w", err)
	}

	if err := json.Unmarshal([]byte(stickers), &desk.Stickers); err != nil {
		return Desk{}, fmt.Errorf("decode assistant stickers: %w", err)
	}

	desk.LastTurnAt = last.Time

	return desk, nil
}

// SaveDeskTx commits slots alongside the input receipt and user-visible reply.
func SaveDeskTx(ctx context.Context, tx *sql.Tx, sessionID int64, desk Desk) error {
	if desk.Diary == nil {
		desk.Diary = map[string]string{}
	}

	if desk.Stickers == nil {
		desk.Stickers = []Sticker{}
	}

	diary, err := json.Marshal(desk.Diary)
	if err != nil {
		return fmt.Errorf("encode assistant diary: %w", err)
	}

	stickers, err := json.Marshal(desk.Stickers)
	if err != nil {
		return fmt.Errorf("encode assistant stickers: %w", err)
	}

	_, err = tx.ExecContext(ctx, `INSERT INTO assistant_desk
		(session_id, diary, stickers, focus, last_turn_at, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET diary = excluded.diary, stickers = excluded.stickers,
		focus = excluded.focus, last_turn_at = excluded.last_turn_at, updated_at = excluded.updated_at`,
		sessionID, string(diary), string(stickers), desk.Focus, nullableTime(desk.LastTurnAt), desk.UpdatedAt)
	if err != nil {
		return fmt.Errorf("save assistant desk: %w", err)
	}

	return nil
}

// RecentTape returns the newest lines, hidden ones included, oldest first.
func (s *store) RecentTape(ctx context.Context, sessionID int64, limit int) ([]Line, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, kind, text FROM (
		SELECT id, at, kind, text FROM assistant_tape WHERE session_id = ? ORDER BY id DESC LIMIT ?
	) ORDER BY id`, sessionID, max(0, limit))
	if err != nil {
		return nil, fmt.Errorf("load recent assistant tape: %w", err)
	}
	defer rows.Close()

	var lines []Line

	for rows.Next() {
		var line Line
		if err := rows.Scan(&line.ID, &line.At, &line.Kind, &line.Text); err != nil {
			return nil, fmt.Errorf("scan recent assistant tape: %w", err)
		}

		lines = append(lines, line)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read recent assistant tape: %w", err)
	}

	return lines, nil
}

// VisibleTape includes a pinned event even outside the window or hidden ranges.
func (s *store) VisibleTape(ctx context.Context, sessionID int64, limit int, pinnedID int64) ([]Line, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, kind, text FROM assistant_tape
		WHERE session_id = ? AND (id = ? OR id IN (
			SELECT id FROM assistant_tape WHERE session_id = ? AND hidden = 0 ORDER BY id DESC LIMIT ?
		)) ORDER BY id`, sessionID, pinnedID, sessionID, max(0, limit))
	if err != nil {
		return nil, fmt.Errorf("load assistant tape: %w", err)
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

// AppendTapeTx never reuses a committed line's number.
func AppendTapeTx(ctx context.Context, tx *sql.Tx, sessionID int64, line Line) (int64, error) {
	result, err := tx.ExecContext(ctx, `INSERT INTO assistant_tape (session_id, at, kind, text)
		VALUES (?, ?, ?, ?)`, sessionID, line.At, line.Kind, line.Text)
	if err != nil {
		return 0, fmt.Errorf("append assistant tape: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("assistant tape id: %w", err)
	}

	return id, nil
}

// HideTapeTx retires inclusive ranges without deleting history.
func HideTapeTx(ctx context.Context, tx *sql.Tx, sessionID int64, ranges [][2]int64) error {
	for _, span := range ranges {
		if _, err := tx.ExecContext(ctx, `UPDATE assistant_tape SET hidden = 1
			WHERE session_id = ? AND id BETWEEN ? AND ?`, sessionID, span[0], span[1]); err != nil {
			return fmt.Errorf("hide assistant tape: %w", err)
		}
	}

	return nil
}

func nullableTime(at time.Time) any {
	if at.IsZero() {
		return nil
	}

	return at
}
