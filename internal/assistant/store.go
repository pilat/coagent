package assistant

import (
	"cmp"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/pilat/coagent/internal/sessionstore"
)

const (
	userKind = "user"
	sayKind  = "say"
	// conversationLines bounds the dialog the desk repeats; older lines are found by search.
	conversationLines = 40
)

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

// Focus refs identify the work sessions this item waits on.
type Focus struct {
	ID        int64
	Text      string
	Sessions  []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Line is one accepted user input or published model message.
type Line struct {
	ID   int64
	At   time.Time
	Kind string
	Text string
}

// Scope names the assistant root and the manager whose work roots it may address.
type Scope struct {
	Root  int64
	Owner string
}

// Store owns the assistant's durable state and its reads of the work roots it may address.
type Store struct {
	db           *sql.DB
	sessions     *sessionstore.Store
	projectsRoot string
}

// New shares the session database without owning its lifetime.
func New(db *sql.DB, sessions *sessionstore.Store, projectsRoot string) *Store {
	return &Store{db: db, sessions: sessions, projectsRoot: projectsRoot}
}

// Live reports whether the note still belongs on the desk on the given UTC date.
func (m Memory) Live(today string) bool {
	return m.Until == "" || m.Until >= today
}

// LoadDesk reads bounded conversation history and the current live notes.
func (s *Store) LoadDesk(ctx context.Context, root int64, now time.Time) (Desk, error) {
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

	desk.Conversation, desk.Spoken, err = s.conversation(ctx, root)
	if err != nil {
		return Desk{}, fmt.Errorf("load assistant conversation: %w", err)
	}

	return desk, nil
}

// Memory includes expired notes: search can still surface them for rewriting or removal.
func (s *Store) Memory(ctx context.Context, root int64) ([]Memory, error) {
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
func (s *Store) Focus(ctx context.Context, root int64) ([]Focus, error) {
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
