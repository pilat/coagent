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

	if _, err := tx.ExecContext(
		ctx,
		`UPDATE sessions SET status = 'error', updated_at = ? WHERE id = ?`,
		time.Now().UTC(),
		sessionID,
	); err != nil {
		return false, fmt.Errorf("park failed session: %w", err)
	}

	owner, _ := record.Attributes[managerIDAttribute].(string)

	reported := record.Status != SessionStatusError
	if owner != "" && record.ParentID == 0 {
		reported, err = recordStartFailureOutputTx(ctx, tx, record, content)
		if err != nil {
			return false, err
		}
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit start failure: %w", err)
	}

	return reported, nil
}

func recordStartFailureOutputTx(ctx context.Context, tx *sql.Tx, record *SessionRecord, content string) (bool, error) {
	var inputID int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(id), 0) FROM session_inbox WHERE session_id = ? AND state = 'pending'`, record.ID).
		Scan(&inputID); err != nil {
		return false, fmt.Errorf("identify failed input: %w", err)
	}

	key := fmt.Sprintf("session-start-failure:generation:%d", record.ModelInputGeneration)
	if inputID != 0 {
		key = fmt.Sprintf("session-start-failure:input:%d", inputID)
	}

	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_outbox WHERE session_id = ? AND source_key = ?)`, record.ID, key).
		Scan(&exists); err != nil {
		return false, fmt.Errorf("find start failure receipt: %w", err)
	}

	if exists {
		return false, nil
	}

	_, err := insertOutputTx(ctx, tx, OutputDraft{
		SessionID: record.ID, Type: OutputMessagePersistent, Content: content,
		SourceKey: key, Fingerprint: OutputFingerprint(OutputMessagePersistent, content, record.ID, nil),
	}, CommitLoop)
	if err != nil {
		return false, fmt.Errorf("record start failure receipt: %w", err)
	}

	return true, nil
}
