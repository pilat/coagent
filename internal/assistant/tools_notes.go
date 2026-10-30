package assistant

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	memoryNoteChars  = 300
	memoryTotalChars = 4000
	focusTextChars   = 200
	focusLiveItems   = 30
)

func (t *assistantTool) remember(ctx context.Context, raw json.RawMessage) (string, error) {
	var id int64

	err := t.store.sessions.WithTx(ctx, func(tx *sql.Tx) error {
		var args memoryArgs
		if err := decodeArgs(t.ID(), raw, &args); err != nil {
			return err
		}

		now := time.Now().UTC()

		note := Memory{Text: strings.TrimSpace(args.Text), Until: strings.TrimSpace(args.Until)}
		if err := checkMemory(ctx, tx, t.scope.Root, note, now); err != nil {
			return err
		}

		var err error
		id, err = insertMemoryTx(ctx, tx, t.scope.Root, note, now)

		return err
	})

	return noteResult(fmt.Sprintf("m%d", id), err)
}

func (t *assistantTool) rewriteMemory(ctx context.Context, raw json.RawMessage) (string, error) {
	var args memoryArgs

	err := t.store.sessions.WithTx(ctx, func(tx *sql.Tx) error {
		if err := decodeArgs(t.ID(), raw, &args); err != nil {
			return err
		}

		note, err := loadMemoryTx(ctx, tx, t.scope.Root, args.ID)
		if err != nil {
			return err
		}

		note.Text, note.Until = strings.TrimSpace(args.Text), strings.TrimSpace(args.Until)

		now := time.Now().UTC()
		if err := checkMemory(ctx, tx, t.scope.Root, note, now); err != nil {
			return err
		}

		return updateMemoryTx(ctx, tx, t.scope.Root, note, now)
	})

	return noteResult("rewrote memory "+args.ID, err)
}

func (t *assistantTool) forgetMemory(ctx context.Context, raw json.RawMessage) (string, error) {
	var args memoryArgs

	err := t.store.sessions.WithTx(ctx, func(tx *sql.Tx) error {
		if err := decodeArgs(t.ID(), raw, &args); err != nil {
			return err
		}

		note, err := loadMemoryTx(ctx, tx, t.scope.Root, args.ID)
		if err != nil {
			return err
		}

		return deleteMemoryTx(ctx, tx, t.scope.Root, note.ID)
	})

	return noteResult("forgot memory "+args.ID, err)
}

func loadMemoryTx(ctx context.Context, tx *sql.Tx, root int64, ref string) (Memory, error) {
	id, err := noteID(ref, "m")
	if err != nil {
		return Memory{}, err
	}

	var found int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM assistant_memory WHERE id = ? AND session_id = ?`, id, root).
		Scan(&found); err != nil {
		return Memory{}, fmt.Errorf("load memory note: %w", err)
	}

	if found == 0 {
		return Memory{}, fmt.Errorf("no memory note %q", ref)
	}

	return Memory{ID: id}, nil
}

func checkMemory(ctx context.Context, tx *sql.Tx, root int64, note Memory, now time.Time) error {
	if note.Text == "" {
		return errors.New("a memory note needs text")
	}

	if chars := utf8.RuneCountInString(note.Text); chars > memoryNoteChars {
		return fmt.Errorf("a memory note holds at most %d characters, this one has %d", memoryNoteChars, chars)
	}

	if note.Until != "" {
		if _, err := time.Parse(time.DateOnly, note.Until); err != nil {
			return fmt.Errorf("until %q must be a date: %w", note.Until, err)
		}
	}

	today := now.Format(time.DateOnly)

	used, err := memoryUsageTx(ctx, tx, root, note.ID, today)
	if err != nil {
		return err
	}

	if note.Live(today) {
		used += utf8.RuneCountInString(note.Text)
	}

	if used > memoryTotalChars {
		return fmt.Errorf("memory is full (%d/%d): merge, shorten or remove notes first", used, memoryTotalChars)
	}

	return nil
}

func memoryUsageTx(ctx context.Context, tx *sql.Tx, root, except int64, today string) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT text FROM assistant_memory
WHERE session_id = ? AND id <> ? AND (until IS NULL OR until >= ?)`, root, except, today)
	if err != nil {
		return 0, fmt.Errorf("load live memory usage: %w", err)
	}
	defer rows.Close()

	used := 0

	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return 0, fmt.Errorf("scan live memory usage: %w", err)
		}

		used += utf8.RuneCountInString(text)
	}

	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read live memory usage: %w", err)
	}

	return used, nil
}

func (t *assistantTool) addFocus(ctx context.Context, raw json.RawMessage) (string, error) {
	var id int64

	err := t.store.sessions.WithTx(ctx, func(tx *sql.Tx) error {
		var args focusArgs
		if err := decodeArgs(t.ID(), raw, &args); err != nil {
			return err
		}

		item := Focus{Text: strings.TrimSpace(args.Text)}
		if err := checkFocusText(item.Text); err != nil {
			return err
		}

		sessions, err := t.focusSessions(ctx, tx, args.Sessions)
		if err != nil {
			return err
		}

		item.Sessions = sessions

		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM assistant_focus WHERE session_id = ?`, t.scope.Root).
			Scan(&count); err != nil {
			return fmt.Errorf("count live focus: %w", err)
		}

		if count >= focusLiveItems {
			return fmt.Errorf(
				"focus holds at most %d items: finish or remove some with done_focus first",
				focusLiveItems,
			)
		}

		id, err = insertFocusTx(ctx, tx, t.scope.Root, item, time.Now().UTC())

		return err
	})

	return noteResult(fmt.Sprintf("f%d", id), err)
}

func (t *assistantTool) updateFocus(ctx context.Context, raw json.RawMessage) (string, error) {
	var args focusArgs

	err := t.store.sessions.WithTx(ctx, func(tx *sql.Tx) error {
		if err := decodeArgs(t.ID(), raw, &args); err != nil {
			return err
		}

		item, err := loadFocusTx(ctx, tx, t.scope.Root, args.ID)
		if err != nil {
			return err
		}

		item.Text = strings.TrimSpace(args.Text)
		if err := checkFocusText(item.Text); err != nil {
			return err
		}

		if args.Sessions != nil {
			sessions, err := t.focusSessions(ctx, tx, args.Sessions)
			if err != nil {
				return err
			}

			item.Sessions = sessions
		}

		return updateFocusTx(ctx, tx, t.scope.Root, item, time.Now().UTC())
	})

	return noteResult("updated focus "+args.ID, err)
}

func (t *assistantTool) doneFocus(ctx context.Context, raw json.RawMessage) (string, error) {
	var args focusArgs

	err := t.store.sessions.WithTx(ctx, func(tx *sql.Tx) error {
		if err := decodeArgs(t.ID(), raw, &args); err != nil {
			return err
		}

		item, err := loadFocusTx(ctx, tx, t.scope.Root, args.ID)
		if err != nil {
			return err
		}

		return deleteFocusTx(ctx, tx, t.scope.Root, item.ID)
	})

	return noteResult("removed focus "+args.ID, err)
}

func loadFocusTx(ctx context.Context, tx *sql.Tx, root int64, ref string) (Focus, error) {
	id, err := noteID(ref, "f")
	if err != nil {
		return Focus{}, err
	}

	item := Focus{ID: id}
	var refs string

	err = tx.QueryRowContext(ctx, `SELECT sessions FROM assistant_focus WHERE id = ? AND session_id = ?`, id, root).
		Scan(&refs)
	if errors.Is(err, sql.ErrNoRows) {
		return Focus{}, fmt.Errorf("no focus item %q", ref)
	}

	if err != nil {
		return Focus{}, fmt.Errorf("load focus item: %w", err)
	}

	if err := json.Unmarshal([]byte(refs), &item.Sessions); err != nil {
		return Focus{}, fmt.Errorf("decode focus sessions: %w", err)
	}

	return item, nil
}

func (t *assistantTool) focusSessions(ctx context.Context, tx *sql.Tx, refs []sessionRef) ([]string, error) {
	sessions := []string{}

	for _, ref := range refs {
		id, err := ref.parse()
		if err != nil {
			return nil, err
		}

		authorized, err := authorizedTx(ctx, tx, t.scope.Owner, id)
		if err != nil {
			return nil, err
		}

		if !authorized {
			return nil, fmt.Errorf("session %d is not a work session you can address", id)
		}

		value := strconv.FormatInt(id, 10)
		if !slices.Contains(sessions, value) {
			sessions = append(sessions, value)
		}
	}

	return sessions, nil
}

func checkFocusText(text string) error {
	if text == "" || strings.ContainsAny(text, "\r\n") {
		return errors.New("a focus item needs one line of text")
	}

	if chars := utf8.RuneCountInString(text); chars > focusTextChars {
		return fmt.Errorf("a focus item holds at most %d characters, this one has %d", focusTextChars, chars)
	}

	return nil
}

func noteID(ref, prefix string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(ref), prefix), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid %s id %q", prefix, ref)
	}

	return id, nil
}

func noteResult(text string, err error) (string, error) {
	if err != nil {
		return "", fmt.Errorf("edit assistant notes: %w", err)
	}

	return text, nil
}
