package sessionstore

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// DeliverCompletionAtomic CAS-stamps delivered_at for one exact child
// activation and, only on a winning CAS, inserts the completion message(s) and
// stamps delivered_msg_id — all in one transaction so a crash commits both or
// neither. This is the sole dedup for at-least-once delivery: same-activation
// redelivery loses delivered_at, while an older activation loses activation_seq.
//
//nolint:nonamedreturns // three heterogeneous results are ambiguous at call sites without names
func (s *store) DeliverCompletionAtomic(
	ctx context.Context,
	sessionID int64,
	msgs []*StoredMessage,
	childID int64,
	activationSeq int64,
) (msgIDs []int64, won bool, err error) {
	if len(msgs) == 0 {
		return nil, false, fmt.Errorf("deliver completion for child %d: no messages", childID)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, fmt.Errorf("begin tx: %w", err)
	}

	defer func() { _ = tx.Rollback() }()

	now := time.Now().UTC().Unix()

	res, err := tx.ExecContext(
		ctx,
		`UPDATE subagent_links SET delivered_at = ?
		 WHERE child_id = ? AND parent_id = ?
		   AND activation_seq = ? AND delivered_at IS NULL`,
		now, childID, sessionID, activationSeq,
	)
	if err != nil {
		return nil, false, fmt.Errorf("cas delivered_at: %w", err)
	}

	affected, raErr := res.RowsAffected()
	if raErr != nil {
		return nil, false, fmt.Errorf("rows affected: %w", raErr)
	}

	if affected == 0 {
		var actualParentID int64

		scanErr := tx.QueryRowContext(
			ctx,
			`SELECT parent_id FROM subagent_links WHERE child_id = ?`,
			childID,
		).Scan(&actualParentID)
		if scanErr == sql.ErrNoRows {
			return nil, false, fmt.Errorf("subagent link for child %d not found", childID)
		}

		if scanErr != nil {
			return nil, false, fmt.Errorf("load completion link: %w", scanErr)
		}

		if actualParentID != sessionID {
			return nil, false, fmt.Errorf(
				"child %d belongs to parent %d, not session %d",
				childID,
				actualParentID,
				sessionID,
			)
		}

		return nil, false, nil // lost the CAS — deferred rollback discards the no-op
	}

	msgIDs, err = insertCompletionMessages(ctx, tx, sessionID, childID, msgs)
	if err != nil {
		return nil, false, err
	}

	if err := tx.Commit(); err != nil {
		return nil, false, fmt.Errorf("commit tx: %w", err)
	}

	return msgIDs, true, nil
}

// insertCompletionMessages inserts each completion message and stamps the link's
// delivered_msg_id with the last id. Caller holds an open tx that has already won
// the delivered_at CAS.
func insertCompletionMessages(
	ctx context.Context,
	tx *sql.Tx,
	sessionID, childID int64,
	msgs []*StoredMessage,
) ([]int64, error) {
	msgIDs := make([]int64, 0, len(msgs))

	for _, m := range msgs {
		id, err := insertMessageWith(ctx, tx, sessionID, m)
		if err != nil {
			return nil, fmt.Errorf("insert completion message: %w", err)
		}

		msgIDs = append(msgIDs, id)
	}

	if len(msgIDs) == 0 {
		return msgIDs, nil
	}

	if _, err := tx.ExecContext(
		ctx,
		`UPDATE subagent_links SET delivered_msg_id = ? WHERE child_id = ?`,
		msgIDs[len(msgIDs)-1], childID,
	); err != nil {
		return nil, fmt.Errorf("set delivered_msg_id: %w", err)
	}

	return msgIDs, nil
}
