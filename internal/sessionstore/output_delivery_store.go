package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

func (s *store) BindManager(ctx context.Context, managerID, driver string, attributes map[string]any) error {
	if managerID == "" || driver == "" || len(attributes) == 0 {
		return errors.New("manager binding requires manager, driver, and identity")
	}

	if err := validateManagerBinding(driver, attributes); err != nil {
		return err
	}

	encoded, err := json.Marshal(attributes)
	if err != nil || string(encoded) == "{}" {
		return errors.New("manager binding requires object identity")
	}

	now := time.Now().UTC()

	result, err := s.db.ExecContext(ctx, `
		INSERT INTO manager_bindings (manager_id, driver, attributes, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(manager_id) DO UPDATE SET updated_at = excluded.updated_at
		WHERE manager_bindings.driver = excluded.driver AND manager_bindings.attributes = excluded.attributes`,
		managerID, driver, string(encoded), now, now)
	if err != nil {
		return fmt.Errorf("bind manager: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("binding rows affected: %w", err)
	}

	if rows == 0 {
		return fmt.Errorf("%w: manager %q", ErrManagerBinding, managerID)
	}

	return nil
}

func validateManagerBinding(driver string, attributes map[string]any) error {
	switch driver {
	case "cli":
		if len(attributes) != 1 || attributes["local"] != true {
			return fmt.Errorf("%w: invalid cli identity", ErrManagerBinding)
		}
	case "telegram":
		_, botOK := positiveInt64(attributes["bot_user_id"])
		chatOK := validBindingInt64(attributes["chat_id"])

		topology, topologyOK := attributes["topology"].(string)
		if len(attributes) != 3 || !botOK || !chatOK ||
			!topologyOK || (topology != "group" && topology != "bot") {
			return fmt.Errorf("%w: invalid telegram identity", ErrManagerBinding)
		}
	}

	return nil
}

func validBindingInt64(value any) bool {
	switch number := value.(type) {
	case int64:
		return number != 0
	case int:
		return number != 0
	case float64:
		return number != 0 && number == float64(int64(number))
	default:
		return false
	}
}

func (s *store) ClaimOutputHead(ctx context.Context, managerID string) (*OutputClaim, error) {
	if managerID == "" {
		return nil, errors.New("empty manager id")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin output claim: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := requireManagerBinding(ctx, tx, managerID); err != nil {
		return nil, err
	}

	record, err := selectOutputHead(ctx, tx, managerID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNoOutput
	}

	if err != nil {
		return nil, fmt.Errorf("select output head: %w", err)
	}

	if record.State == OutputStateBlocked || record.State == OutputStateDelivering ||
		(record.State == OutputStateRetryWait && record.NextAttemptAt.After(time.Now().UTC())) {
		if record.State == OutputStateRetryWait {
			return nil, &OutputRetryPendingError{NextAt: *record.NextAttemptAt}
		}

		return nil, ErrNoOutput
	}

	attemptID, err := randomAttemptID()
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()

	result, err := tx.ExecContext(ctx, `
		UPDATE session_outbox
		SET state = 'delivering', attempt_seq = attempt_seq + 1, attempt_id = ?,
			last_attempt_at = ?, next_attempt_at = NULL, last_error = ''
		WHERE id = ? AND state IN ('pending', 'retry_wait')`, attemptID, now, record.ID)
	if err != nil {
		return nil, fmt.Errorf("claim output %d: %w", record.ID, err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("claim rows affected: %w", err)
	}

	if rows != 1 {
		return nil, ErrNoOutput
	}

	record.State, record.AttemptID = OutputStateDelivering, attemptID
	record.AttemptSeq++
	record.LastAttemptAt = &now
	record.NextAttemptAt = nil
	record.LastError = ""

	attributes, err := outputSessionAttributes(ctx, tx, record.SessionID)
	if err != nil {
		return nil, err
	}

	previous, err := previousDeliveredMessage(ctx, tx, record.SessionID, record.ID)
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit output claim: %w", err)
	}

	return &OutputClaim{Output: record, SessionAttributes: attributes, PreviousDeliveredOutput: previous}, nil
}

func (s *store) AckOutput(
	ctx context.Context,
	managerID string,
	outputID int64,
	attemptID string,
	messageIDs []string,
	sessionPatch map[string]any,
) error {
	if err := validateMessageIDs(messageIDs); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin output ack: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	record, err := outputForAttempt(ctx, tx, managerID, outputID, attemptID)
	if err != nil {
		return err
	}

	if isMessageOutput(record.Type) && messageIDs == nil {
		return errors.New("message output acknowledgement requires message ids")
	}

	attributes := cloneAttributes(record.Attributes)
	if messageIDs != nil {
		attributes["message_ids"] = messageIDs
	}

	encoded, err := json.Marshal(attributes)
	if err != nil {
		return fmt.Errorf("marshal ack attributes: %w", err)
	}

	if err := patchOutputSessionAttributes(ctx, tx, record.SessionID, managerID, sessionPatch); err != nil {
		return err
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE session_outbox SET state = 'delivered', attempt_id = NULL,
			delivered_at = ?, attributes = ?, last_error = ''
		WHERE id = ? AND state = 'delivering' AND attempt_id = ?`,
		time.Now().UTC(), string(encoded), outputID, attemptID)
	if err != nil {
		return fmt.Errorf("ack output: %w", err)
	}

	if err := requireOutputAttempt(result, outputID); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit output ack: %w", err)
	}

	return nil
}

func (s *store) RetryOutput(
	ctx context.Context,
	managerID string,
	outputID int64,
	attemptID, failure string,
	next time.Time,
) error {
	if failure == "" || len(failure) > 512 || next.IsZero() {
		return errors.New("invalid output retry")
	}

	return s.resolveOutputAttempt(ctx, managerID, outputID, attemptID, failure, OutputStateRetryWait, next)
}

func (s *store) BlockOutput(ctx context.Context, managerID string, outputID int64, attemptID, failure string) error {
	if failure == "" || len(failure) > 512 {
		return errors.New("invalid output block")
	}

	return s.resolveOutputAttempt(ctx, managerID, outputID, attemptID, failure, OutputStateBlocked, time.Time{})
}

func (s *store) RecoverInterruptedOutputs(ctx context.Context) (int64, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE session_outbox SET state = 'retry_wait', attempt_id = NULL,
			next_attempt_at = ?, last_error = 'delivery interrupted by restart'
		WHERE state = 'delivering'`, time.Now().UTC())
	if err != nil {
		return 0, fmt.Errorf("recover interrupted output: %w", err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("recovered output rows affected: %w", err)
	}

	return count, nil
}

func (s *store) RetryBlockedHead(ctx context.Context, managerID string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE session_outbox SET state = 'retry_wait', blocked_at = NULL, next_attempt_at = ?, last_error = 'retry requested after manager start'
		WHERE id = (
			SELECT id FROM session_outbox
			WHERE json_extract(attributes, '$.manager_id') = ? AND state <> 'delivered'
			ORDER BY id LIMIT 1
		) AND state = 'blocked'`, time.Now().UTC(), managerID)
	if err != nil {
		return false, fmt.Errorf("retry blocked output: %w", err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("retry blocked rows affected: %w", err)
	}

	return count == 1, nil
}

// WakeOutputHead makes a retrying manager head immediately eligible. It does
// not alter attempt state, so reconnect never erases the delivery history.
func (s *store) WakeOutputHead(ctx context.Context, managerID string) (bool, error) {
	result, err := s.db.ExecContext(ctx, `
		UPDATE session_outbox SET next_attempt_at = ?
		WHERE id = (
			SELECT id FROM session_outbox
			WHERE json_extract(attributes, '$.manager_id') = ? AND state <> 'delivered'
			ORDER BY id LIMIT 1
		) AND state = 'retry_wait'`, time.Now().UTC(), managerID)
	if err != nil {
		return false, fmt.Errorf("wake output head: %w", err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("wake output rows affected: %w", err)
	}

	return count == 1, nil
}

func (s *store) OutputQueueStatus(ctx context.Context, managerID string) (*OutputQueueStatus, error) {
	status := &OutputQueueStatus{}
	if err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM session_outbox
		WHERE json_extract(attributes, '$.manager_id') = ? AND state <> 'delivered'`, managerID).Scan(&status.Pending); err != nil {
		return nil, fmt.Errorf("count pending outputs: %w", err)
	}

	var blockedAt sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		SELECT id, blocked_at, last_error FROM session_outbox
		WHERE json_extract(attributes, '$.manager_id') = ? AND state = 'blocked'
		ORDER BY id LIMIT 1`, managerID).Scan(&status.BlockedID, &blockedAt, &status.DeliveryError)
	if errors.Is(err, sql.ErrNoRows) {
		return status, nil
	}

	if err != nil {
		return nil, fmt.Errorf("load blocked output: %w", err)
	}

	status.BlockedAt = &blockedAt.Time

	return status, nil
}
