package assistant

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"
)

func insertMemoryTx(ctx context.Context, tx *sql.Tx, root int64, note Memory, now time.Time) (int64, error) {
	result, err := tx.ExecContext(ctx, `INSERT INTO assistant_memory (session_id, text, until, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`, root, note.Text, nullableText(note.Until), now, now)
	if err != nil {
		return 0, fmt.Errorf("insert assistant memory: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("assistant memory id: %w", err)
	}

	return id, indexTx(ctx, tx, searchMemory, id, root, note.Text)
}

func updateMemoryTx(ctx context.Context, tx *sql.Tx, root int64, note Memory, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE assistant_memory SET text = ?, until = ?, updated_at = ?
		WHERE id = ? AND session_id = ?`, note.Text, nullableText(note.Until), now, note.ID, root); err != nil {
		return fmt.Errorf("update assistant memory %d: %w", note.ID, err)
	}

	return indexTx(ctx, tx, searchMemory, note.ID, root, note.Text)
}

// deleteMemoryTx removes the note from search too: a wrong fact must not be found again.
func deleteMemoryTx(ctx context.Context, tx *sql.Tx, root, id int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM assistant_memory WHERE id = ? AND session_id = ?`,
		id, root); err != nil {
		return fmt.Errorf("delete assistant memory %d: %w", id, err)
	}

	return unindexTx(ctx, tx, searchMemory, id)
}

func insertFocusTx(ctx context.Context, tx *sql.Tx, root int64, item Focus, now time.Time) (int64, error) {
	refs, err := focusRefs(item.Sessions)
	if err != nil {
		return 0, err
	}

	result, err := tx.ExecContext(ctx, `INSERT INTO assistant_focus (session_id, text, sessions, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)`, root, item.Text, refs, now, now)
	if err != nil {
		return 0, fmt.Errorf("insert assistant focus: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("assistant focus id: %w", err)
	}

	return id, indexTx(ctx, tx, searchFocus, id, root, item.Text)
}

func updateFocusTx(ctx context.Context, tx *sql.Tx, root int64, item Focus, now time.Time) error {
	refs, err := focusRefs(item.Sessions)
	if err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `UPDATE assistant_focus SET text = ?, sessions = ?, updated_at = ?
		WHERE id = ? AND session_id = ?`, item.Text, refs, now, item.ID, root); err != nil {
		return fmt.Errorf("update assistant focus %d: %w", item.ID, err)
	}

	return indexTx(ctx, tx, searchFocus, item.ID, root, item.Text)
}

func deleteFocusTx(ctx context.Context, tx *sql.Tx, root, id int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM assistant_focus WHERE id = ? AND session_id = ?`,
		id, root); err != nil {
		return fmt.Errorf("delete assistant focus %d: %w", id, err)
	}

	return unindexTx(ctx, tx, searchFocus, id)
}

func focusRefs(sessions []string) (string, error) {
	if sessions == nil {
		sessions = []string{}
	}

	data, err := json.Marshal(sessions)
	if err != nil {
		return "", fmt.Errorf("encode assistant focus sessions: %w", err)
	}

	return string(data), nil
}

func nullableText(text string) any {
	if text == "" {
		return nil
	}

	return text
}
