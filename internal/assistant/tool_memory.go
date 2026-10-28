package assistant

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pilat/coagent/internal/llmwire"
)

func (t *assistantTool) edit(ctx context.Context, call llmwire.ToolCall) (string, error) {
	var text string

	err := t.store.sessions.WithTx(ctx, func(tx *sql.Tx) error {
		var err error

		switch call.Name {
		case toolRemember, toolRewriteMemory, toolForgetMemory:
			text, err = t.editMemory(ctx, tx, call)
		default:
			text, err = t.editFocus(ctx, tx, call)
		}

		return err
	})
	if err != nil {
		return "", fmt.Errorf("edit assistant notes: %w", err)
	}

	return text, nil
}

func (t *assistantTool) editMemory(ctx context.Context, tx *sql.Tx, call llmwire.ToolCall) (string, error) {
	var args memoryArgs
	if err := decodeArgs(call, &args); err != nil {
		return "", err
	}

	now := time.Now().UTC()

	note := Memory{Text: strings.TrimSpace(args.Text), Until: strings.TrimSpace(args.Until)}
	if call.Name != toolRemember {
		id, err := noteID(args.ID, "m")
		if err != nil {
			return "", err
		}

		note.ID = id

		var found int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM assistant_memory WHERE id = ? AND session_id = ?`, id, t.scope.Root).
			Scan(&found); err != nil {
			return "", fmt.Errorf("load memory note: %w", err)
		}

		if found == 0 {
			return "", fmt.Errorf("no memory note %q", args.ID)
		}
	}

	if call.Name == toolForgetMemory {
		return "forgot memory " + args.ID, deleteMemoryTx(ctx, tx, t.scope.Root, note.ID)
	}

	if err := checkMemory(ctx, tx, t.scope.Root, note, now); err != nil {
		return "", err
	}

	if call.Name == toolRemember {
		id, err := insertMemoryTx(ctx, tx, t.scope.Root, note, now)
		return fmt.Sprintf("m%d", id), err
	}

	return "rewrote memory " + args.ID, updateMemoryTx(ctx, tx, t.scope.Root, note, now)
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
