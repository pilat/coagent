package sessionstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func outputOwner(ctx context.Context, tx *sql.Tx, sessionID int64) (string, error) {
	var parentID int64
	var attributes string

	err := tx.QueryRowContext(ctx, `SELECT parent_id, attributes FROM sessions WHERE id = ?`, sessionID).
		Scan(&parentID, &attributes)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("session %d not found", sessionID)
	}

	if err != nil {
		return "", fmt.Errorf("load output owner: %w", err)
	}

	if parentID != 0 {
		return "", ErrOutputNotRoot
	}

	var values map[string]any
	if err := json.Unmarshal([]byte(attributes), &values); err != nil {
		return "", fmt.Errorf("decode session attributes: %w", err)
	}

	owner, _ := values[managerIDAttribute].(string)
	if owner == "" {
		return "", ErrOutputOwner
	}

	return owner, nil
}

func outputSessionWritable(ctx context.Context, tx *sql.Tx, sessionID int64) error {
	var status SessionStatus

	var killedAt sql.NullTime
	if err := tx.QueryRowContext(ctx, `SELECT status, killed_at FROM sessions WHERE id = ?`, sessionID).
		Scan(&status, &killedAt); err != nil {
		return fmt.Errorf("load output session state: %w", err)
	}

	if killedAt.Valid || status == SessionStatusStopping || status == SessionStatusTerminating ||
		status == SessionStatusKilled {
		return fmt.Errorf("session %d cannot commit ordinary output", sessionID)
	}

	return nil
}

func selectOutputHead(ctx context.Context, tx *sql.Tx, managerID string) (*OutputRecord, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+outputColumns+` FROM session_outbox
		WHERE json_extract(attributes, '$.manager_id') = ? AND state <> 'delivered'
		ORDER BY id LIMIT 1`, managerID)

	return scanOutputRecord(row)
}

const outputColumns = `id, session_id, type, content, attributes, COALESCE(source_key, ''), COALESCE(fingerprint, ''), state, attempt_seq, COALESCE(attempt_id, ''), last_attempt_at, next_attempt_at, delivered_at, blocked_at, last_error, created_at, releases_input`

func scanOutputRecord(row interface{ Scan(...any) error }) (*OutputRecord, error) {
	var record OutputRecord
	var encoded string
	var lastAttempt, nextAttempt, delivered, blocked sql.NullTime

	err := row.Scan(
		&record.ID,
		&record.SessionID,
		&record.Type,
		&record.Content,
		&encoded,
		&record.SourceKey,
		&record.Fingerprint,
		&record.State,
		&record.AttemptSeq,
		&record.AttemptID,
		&lastAttempt,
		&nextAttempt,
		&delivered,
		&blocked,
		&record.LastError,
		&record.CreatedAt,
		&record.ReleasesInput,
	)
	if err != nil {
		return nil, fmt.Errorf("scan output record: %w", err)
	}

	if err := json.Unmarshal([]byte(encoded), &record.Attributes); err != nil {
		return nil, fmt.Errorf("decode output attributes: %w", err)
	}

	record.LastAttemptAt = nullTime(lastAttempt)
	record.NextAttemptAt = nullTime(nextAttempt)
	record.DeliveredAt = nullTime(delivered)
	record.BlockedAt = nullTime(blocked)

	return &record, nil
}

func nullTime(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}

	return &value.Time
}

func requireManagerBinding(ctx context.Context, tx *sql.Tx, managerID string) error {
	var found string

	err := tx.QueryRowContext(ctx, `SELECT manager_id FROM manager_bindings WHERE manager_id = ?`, managerID).
		Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: manager %q is not bound", ErrManagerBinding, managerID)
	}

	if err != nil {
		return fmt.Errorf("load manager binding: %w", err)
	}

	return nil
}

func randomAttemptID() (string, error) {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", fmt.Errorf("random output attempt id: %w", err)
	}

	return hex.EncodeToString(bytes), nil
}

func outputSessionAttributes(ctx context.Context, tx *sql.Tx, sessionID int64) (map[string]any, error) {
	var encoded string
	if err := tx.QueryRowContext(ctx, `SELECT attributes FROM sessions WHERE id = ?`, sessionID).
		Scan(&encoded); err != nil {
		return nil, fmt.Errorf("load output session attributes: %w", err)
	}

	attributes := make(map[string]any)
	if err := json.Unmarshal([]byte(encoded), &attributes); err != nil {
		return nil, fmt.Errorf("decode output session attributes: %w", err)
	}

	return attributes, nil
}

func previousDeliveredMessage(ctx context.Context, tx *sql.Tx, sessionID, outputID int64) (*OutputRecord, error) {
	record, err := scanOutputRecord(tx.QueryRowContext(ctx, `SELECT `+outputColumns+` FROM session_outbox
		WHERE session_id = ? AND id < ? AND state = 'delivered'
			AND type IN ('message_replaceable', 'message_persistent')
		ORDER BY id DESC LIMIT 1`, sessionID, outputID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil //nolint:nilnil // no preceding delivered message is a valid receipt-chain start.
	}

	if err != nil {
		return nil, fmt.Errorf("load previous delivered output: %w", err)
	}

	return record, nil
}

func outputForAttempt(
	ctx context.Context,
	tx *sql.Tx,
	managerID string,
	outputID int64,
	attemptID string,
) (*OutputRecord, error) {
	if attemptID == "" {
		return nil, ErrOutputAttempt
	}

	record, err := scanOutputRecord(tx.QueryRowContext(ctx, `SELECT `+outputColumns+` FROM session_outbox
		WHERE id = ? AND json_extract(attributes, '$.manager_id') = ?`, outputID, managerID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrOutputAttempt
	}

	if err != nil {
		return nil, fmt.Errorf("load output attempt: %w", err)
	}

	if record.State != OutputStateDelivering || record.AttemptID != attemptID {
		return nil, ErrOutputAttempt
	}

	return record, nil
}

func (s *store) resolveOutputAttempt(
	ctx context.Context,
	managerID string,
	outputID int64,
	attemptID, failure string,
	target OutputState,
	next time.Time,
) error {
	if attemptID == "" {
		return ErrOutputAttempt
	}

	query := `UPDATE session_outbox SET state = ?, attempt_id = NULL, last_error = ?, next_attempt_at = ?, blocked_at = ? WHERE id = ? AND state = 'delivering' AND attempt_id = ? AND json_extract(attributes, '$.manager_id') = ?`
	var retryAt any
	var blockedAt any

	if target == OutputStateRetryWait {
		retryAt = next.UTC()
	} else {
		blockedAt = time.Now().UTC()
	}

	result, err := s.db.ExecContext(ctx, query, target, failure, retryAt, blockedAt, outputID, attemptID, managerID)
	if err != nil {
		return fmt.Errorf("resolve output attempt: %w", err)
	}

	return requireOutputAttempt(result, outputID)
}

func requireOutputAttempt(result sql.Result, outputID int64) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("output attempt rows affected: %w", err)
	}

	if rows != 1 {
		return fmt.Errorf("%w: output %d", ErrOutputAttempt, outputID)
	}

	return nil
}
