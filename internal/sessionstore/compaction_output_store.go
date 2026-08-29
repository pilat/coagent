package sessionstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// CompactionCommandStore atomically records a compact command's durable
// transcript replacement, terminal inbox state, and visible outcome.
type CompactionCommandStore interface {
	CompleteCompactionInput(
		ctx context.Context,
		inputID, sessionID int64,
		compactedIDs []int64,
		entries []CompactionEntry,
		content string,
	) ([]int64, *OutputCommit, error)
}

func (s *store) CompleteCompactionInput(
	ctx context.Context,
	inputID, sessionID int64,
	compactedIDs []int64,
	entries []CompactionEntry,
	content string,
) ([]int64, *OutputCommit, error) {
	if content == "" {
		return nil, nil, errors.New("empty compaction outcome")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin compaction command: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	input, err := loadInboxInput(ctx, tx, inputID)
	if err != nil {
		return nil, nil, err
	}

	if input.State != InputStatePending || input.SessionID != sessionID {
		return nil, nil, fmt.Errorf("%w: compact input %d", ErrInputResolved, inputID)
	}

	owner, err := outputOwner(ctx, tx, sessionID)
	if err != nil {
		return nil, nil, err
	}

	now := time.Now().UTC()

	ids, err := replaceCompactedMessagesTx(ctx, tx, sessionID, compactedIDs, entries, now)
	if err != nil {
		return nil, nil, err
	}

	result, err := tx.ExecContext(ctx, `UPDATE session_inbox
		SET state = 'handled', resolved_at = ?, resolution_reason = 'compact command'
		WHERE id = ? AND state = 'pending'`, now, inputID)
	if err != nil {
		return nil, nil, fmt.Errorf("handle compaction input: %w", err)
	}

	if err := requireOnePendingResolution(ctx, tx, result, inputID); err != nil {
		return nil, nil, err
	}

	attributes, err := json.Marshal(map[string]any{managerIDAttribute: owner})
	if err != nil {
		return nil, nil, fmt.Errorf("marshal compaction output attributes: %w", err)
	}

	key := fmt.Sprintf("input:%d:compact:succeeded", inputID)
	fingerprint := outputFingerprint(OutputMessagePersistent, content, sessionID, nil)

	result, err = tx.ExecContext(ctx, `INSERT INTO session_outbox
		(session_id, type, content, attributes, source_key, fingerprint, created_at)
		VALUES (?, 'message_persistent', ?, ?, ?, ?, ?)`,
		sessionID, content, string(attributes), key, fingerprint, now)
	if err != nil {
		return nil, nil, fmt.Errorf("insert compaction outcome: %w", err)
	}

	outputID, err := result.LastInsertId()
	if err != nil {
		return nil, nil, fmt.Errorf("compaction outcome id: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit compaction command: %w", err)
	}

	return ids, &OutputCommit{OutputID: outputID, OwnerID: owner}, nil
}
