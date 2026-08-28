package sessionstore

import (
	"context"
	"database/sql"
	"encoding/json"
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

	if err := startScheduledEpisode(ctx, tx, sessionID); err != nil {
		return 0, 0, false, err
	}

	asstID, err = insertMessageWith(ctx, tx, sessionID, assistant)
	if err != nil {
		return 0, 0, false, fmt.Errorf("insert idempotent assistant stub: %w", err)
	}

	resultID, err = insertMessageWith(ctx, tx, sessionID, toolResult)
	if err != nil {
		return 0, 0, false, fmt.Errorf("insert idempotent tool result: %w", err)
	}

	if err := insertScheduledOutput(ctx, tx, sessionID, deliveryID, toolResult.Content); err != nil {
		return 0, 0, false, err
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

	if err := startScheduledEpisode(ctx, tx, sessionID); err != nil {
		return nil, false, err
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

	if err := insertScheduledOutput(ctx, tx, sessionID, deliveryID, opening[len(opening)-1].Content); err != nil {
		return nil, false, err
	}

	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit context reset: %w", err)
	}

	return ids, true, nil
}

func startScheduledEpisode(ctx context.Context, tx *sql.Tx, sessionID int64) error {
	now := time.Now().UTC()

	_, err := tx.ExecContext(ctx, `UPDATE sessions SET episode_started_at = ?
		WHERE id = ? AND parent_id = 0
			AND json_type(attributes, '$.manager_id') = 'text'
			AND json_extract(attributes, '$.manager_id') <> ''
			AND (episode_started_at IS NULL OR status NOT IN ('active', 'suspended'))`, now, sessionID)
	if err != nil {
		return fmt.Errorf("start scheduled episode: %w", err)
	}

	return nil
}

func insertScheduledOutput(ctx context.Context, tx *sql.Tx, sessionID int64, deliveryID, content string) error {
	var parentID int64

	var encodedSessionAttrs string
	if err := tx.QueryRowContext(ctx, `SELECT parent_id, attributes FROM sessions WHERE id = ?`, sessionID).
		Scan(&parentID, &encodedSessionAttrs); err != nil {
		return fmt.Errorf("load scheduled output owner: %w", err)
	}

	if parentID != 0 {
		return nil
	}

	var sessionAttrs map[string]any
	if err := json.Unmarshal([]byte(encodedSessionAttrs), &sessionAttrs); err != nil {
		return fmt.Errorf("decode scheduled output attributes: %w", err)
	}

	owner, _ := sessionAttrs[managerIDAttribute].(string)
	if owner == "" {
		return nil
	}

	attrs := map[string]any{managerIDAttribute: owner, "source": outputSourceScheduler}

	encodedAttrs, err := json.Marshal(attrs)
	if err != nil {
		return fmt.Errorf("marshal scheduled output attributes: %w", err)
	}

	output := "⏰ scheduled\n\n" + content
	key := "schedule:" + deliveryID + ":announcement"

	fingerprint := outputFingerprint(
		OutputMessagePersistent,
		output,
		sessionID,
		map[string]any{"source": outputSourceScheduler},
	)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO session_outbox (session_id, type, content, attributes, source_key, fingerprint, created_at)
		VALUES (?, 'message_persistent', ?, ?, ?, ?, ?)`, sessionID, output, string(encodedAttrs), key, fingerprint, time.Now().UTC()); err != nil {
		return fmt.Errorf("insert scheduled output: %w", err)
	}

	return nil
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
