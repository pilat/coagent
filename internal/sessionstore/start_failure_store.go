package sessionstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// RecordSessionStartFailure parks failed work without consuming input and records
// its first error once, including across process restarts and delivery retries.
func (s *Store) RecordSessionStartFailure(ctx context.Context, sessionID int64, content string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("begin start failure: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	record, err := scanSession(
		tx.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, sessionID),
	)
	if err != nil {
		return false, fmt.Errorf("load failed session: %w", err)
	}

	if record.KilledAt != nil || record.Status == SessionStatusStopping || record.Status == SessionStatusStopped ||
		record.Status == SessionStatusTerminating || record.Status == SessionStatusKilled {
		return false, nil
	}

	now := time.Now().UTC()
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE sessions SET status = 'error', updated_at = ? WHERE id = ?`,
		now,
		sessionID,
	); err != nil {
		return false, fmt.Errorf("park failed session: %w", err)
	}

	owner, _ := record.Attributes[managerIDAttribute].(string)

	reported := record.Status != SessionStatusError
	if owner != "" && record.ParentID == 0 {
		reported, err = recordStartFailureOutputTx(ctx, tx, record, content, now)
		if err != nil {
			return false, err
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit start failure: %w", err)
	}

	return reported, nil
}

func recordStartFailureOutputTx(
	ctx context.Context,
	tx *sql.Tx,
	record *SessionRecord,
	content string,
	at time.Time,
) (bool, error) {
	var inputID int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(id), 0) FROM session_inbox WHERE session_id = ? AND state = 'pending'`, record.ID).
		Scan(&inputID); err != nil {
		return false, fmt.Errorf("identify failed input: %w", err)
	}

	key := fmt.Sprintf("session-start-failure:generation:%d", record.ModelInputGeneration)
	if inputID != 0 {
		key = fmt.Sprintf("session-start-failure:input:%d", inputID)
	}

	out, err := insertOutputTx(ctx, tx, OutputDraft{
		SessionID: record.ID, Type: OutputMessage, Content: content,
		SourceKey: key, CreatedAt: at,
	}, CommitLoop)
	if err != nil {
		return false, fmt.Errorf("record start failure receipt: %w", err)
	}

	if out != nil && out.Existing {
		return false, nil
	}

	return true, nil
}
