package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var ErrDeliveryConflict = errors.New("session delivery identity conflict")

const (
	deliveryKindToolNotification = "tool_notification"
	deliveryKindContextReset     = "context_reset"
)

//nolint:nonamedreturns // two same-typed int64 results are ambiguous at call sites without names
func (s *store) InsertToolNotificationPairOnce(
	ctx context.Context,
	sessionID int64,
	deliveryID, fingerprint string,
	assistant, toolResult *StoredMessage,
) (asstID, resultID int64, inserted bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, false, fmt.Errorf("begin idempotent notification: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	claimed, err := claimSessionDelivery(
		ctx, tx, sessionID, deliveryID, deliveryKindToolNotification, fingerprint,
	)
	if err != nil {
		return 0, 0, false, err
	}

	if !claimed {
		return 0, 0, false, nil
	}

	asstID, err = insertMessageWith(ctx, tx, sessionID, assistant)
	if err != nil {
		return 0, 0, false, fmt.Errorf("insert idempotent assistant stub: %w", err)
	}

	resultID, err = insertMessageWith(ctx, tx, sessionID, toolResult)
	if err != nil {
		return 0, 0, false, fmt.Errorf("insert idempotent tool result: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, false, fmt.Errorf("commit idempotent notification: %w", err)
	}

	return asstID, resultID, true, nil
}

func (s *store) ResetSessionContextOnce(
	ctx context.Context,
	sessionID int64,
	deliveryID, fingerprint string,
	opening []*StoredMessage,
) ([]int64, bool, error) {
	if len(opening) == 0 {
		return nil, false, errors.New("reset session context requires an opening turn")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin context reset: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	claimed, err := claimSessionDelivery(
		ctx, tx, sessionID, deliveryID, deliveryKindContextReset, fingerprint,
	)
	if err != nil {
		return nil, false, err
	}

	if !claimed {
		return nil, false, nil
	}

	now := time.Now().UTC()
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE messages SET compacted_at = ?
		 WHERE session_id = ? AND compacted_at IS NULL`,
		now,
		sessionID,
	); err != nil {
		return nil, false, fmt.Errorf("hide previous transcript: %w", err)
	}

	ids := make([]int64, len(opening))
	for i, message := range opening {
		id, insertErr := insertMessageWith(ctx, tx, sessionID, message)
		if insertErr != nil {
			return nil, false, fmt.Errorf("insert context reset message %d: %w", i, insertErr)
		}

		ids[i] = id
	}

	result, err := tx.ExecContext(
		ctx,
		`UPDATE sessions SET compaction_brief = '', todo_items = '[]' WHERE id = ?`,
		sessionID,
	)
	if err != nil {
		return nil, false, fmt.Errorf("clear context-derived session state: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return nil, false, fmt.Errorf("context reset session rows affected: %w", err)
	}

	if rows != 1 {
		return nil, false, fmt.Errorf("session %d not found during context reset", sessionID)
	}

	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit context reset: %w", err)
	}

	return ids, true, nil
}

func claimSessionDelivery(
	ctx context.Context,
	tx *sql.Tx,
	sessionID int64,
	deliveryID, kind, fingerprint string,
) (bool, error) {
	if deliveryID == "" || fingerprint == "" {
		return false, errors.New("session delivery requires identity and fingerprint")
	}

	result, err := tx.ExecContext(
		ctx,
		`INSERT INTO session_deliveries
			(session_id, delivery_id, kind, fingerprint, delivered_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(session_id, delivery_id) DO NOTHING`,
		sessionID,
		deliveryID,
		kind,
		fingerprint,
		time.Now().UTC(),
	)
	if err != nil {
		return false, fmt.Errorf("claim session delivery: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("session delivery rows affected: %w", err)
	}

	if rows == 1 {
		return true, nil
	}

	var storedKind, storedFingerprint string
	if err := tx.QueryRowContext(
		ctx,
		`SELECT kind, fingerprint FROM session_deliveries
		 WHERE session_id = ? AND delivery_id = ?`,
		sessionID,
		deliveryID,
	).Scan(&storedKind, &storedFingerprint); err != nil {
		return false, fmt.Errorf("load existing session delivery: %w", err)
	}

	if storedKind != kind || storedFingerprint != fingerprint {
		return false, fmt.Errorf(
			"%w: session %d delivery %q was %s/%s, got %s/%s",
			ErrDeliveryConflict,
			sessionID,
			deliveryID,
			storedKind,
			storedFingerprint,
			kind,
			fingerprint,
		)
	}

	return false, nil
}
