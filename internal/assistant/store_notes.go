package assistant

import (
	"context"
	"database/sql"
	"fmt"
	"maps"
	"slices"
	"time"
)

// Notes returns every scope, including unresolved structural bindings.
func (s *store) Notes(ctx context.Context, root int64) ([]Note, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, text, written_at FROM assistant_notes
		WHERE session_id = ? ORDER BY key`, root)
	if err != nil {
		return nil, fmt.Errorf("load assistant notes: %w", err)
	}
	defer rows.Close()
	var notes []Note

	for rows.Next() {
		var note Note
		if err := rows.Scan(&note.Key, &note.Text, &note.WrittenAt); err != nil {
			return nil, fmt.Errorf("scan assistant note: %w", err)
		}

		notes = append(notes, note)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read assistant notes: %w", err)
	}

	return notes, nil
}

// SaveNotesTx stamps writes while retaining absent keys and deleting null keys.
func SaveNotesTx(ctx context.Context, tx *sql.Tx, root int64, edits map[string]*string, now time.Time) error {
	for _, key := range slices.Sorted(maps.Keys(edits)) {
		var err error
		if text := edits[key]; text == nil {
			_, err = tx.ExecContext(ctx, `DELETE FROM assistant_notes WHERE session_id = ? AND key = ?`, root, key)
		} else {
			_, err = tx.ExecContext(ctx, `INSERT INTO assistant_notes (session_id, key, text, written_at)
				VALUES (?, ?, ?, ?) ON CONFLICT(session_id, key)
				DO UPDATE SET text = excluded.text, written_at = excluded.written_at`, root, key, *text, now.Unix())
		}

		if err != nil {
			return fmt.Errorf("save assistant note %s: %w", key, err)
		}
	}

	return nil
}

func rekeyNoteTx(ctx context.Context, tx *sql.Tx, root int64, from, to string) error {
	if from == to {
		return nil
	}

	_, err := tx.ExecContext(ctx, `INSERT INTO assistant_notes (session_id, key, text, written_at)
		SELECT session_id, ?, text, written_at FROM assistant_notes WHERE session_id = ? AND key = ?
		ON CONFLICT(session_id, key) DO UPDATE SET text = excluded.text, written_at = excluded.written_at`, to, root, from)
	if err != nil {
		return fmt.Errorf("rekey assistant note %s: %w", from, err)
	}

	_, err = tx.ExecContext(ctx, `DELETE FROM assistant_notes WHERE session_id = ? AND key = ?`, root, from)
	if err != nil {
		return fmt.Errorf("remove raw assistant note %s: %w", from, err)
	}

	return nil
}
