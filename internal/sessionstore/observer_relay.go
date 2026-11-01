package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const (
	RelayedSessionAttribute = "relayed_session_id"
	RelayedOutputAttribute  = "relayed_output_id"
)

type relayOutput struct {
	id      int64
	content string
}

func relayObservedTx(
	ctx context.Context,
	tx *sql.Tx,
	work, final, output int64,
	notice string,
	window observerWindow,
	at time.Time,
	mode CommitMode,
) error {
	err := outputSessionWritable(ctx, tx, window.observer)
	if errors.Is(err, ErrOutputNotWritable) {
		return nil
	}

	if err != nil {
		return err
	}

	var focused bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM chat_focus f
		JOIN sessions s ON json_extract(s.attributes, '$.manager_id') = f.manager_id
		WHERE s.id = ? AND f.session_id = ?)`, window.observer, work).Scan(&focused); err != nil {
		return fmt.Errorf("read observer chat focus: %w", err)
	}

	if focused {
		return nil
	}

	outputs := []relayOutput{{id: output, content: notice}}

	if final != 0 {
		var err error

		outputs, err = observerFinalOutputsTx(ctx, tx, work, window.lower, final)
		if err != nil {
			return err
		}
	}

	for _, row := range outputs {
		_, err := insertOutputTx(ctx, tx, OutputDraft{
			SessionID: window.observer, Type: OutputMessage,
			Content: row.content, SourceKey: fmt.Sprintf("relay:%d", row.id), CreatedAt: at,
			Attributes: map[string]any{RelayedSessionAttribute: work, RelayedOutputAttribute: row.id},
		}, mode)
		if err != nil {
			return err
		}
	}

	return nil
}

func observerFinalOutputsTx(ctx context.Context, tx *sql.Tx, work, lower, final int64) ([]relayOutput, error) {
	rows, err := tx.QueryContext(ctx, `SELECT o.id,o.content FROM session_outbox o
		JOIN messages m ON m.session_id = o.session_id AND o.source_key = 'message:' || m.id || ':final'
		WHERE m.session_id = ? AND m.id > ? AND m.id <= ? AND m.rejected_reason IS NULL
		AND m.compacted_at IS NULL AND `+FinalMessageSQL+` ORDER BY o.id`, work, lower, final)
	if err != nil {
		return nil, fmt.Errorf("load observed final outputs: %w", err)
	}
	defer rows.Close()
	var outputs []relayOutput

	for rows.Next() {
		var row relayOutput
		if err := rows.Scan(&row.id, &row.content); err != nil {
			return nil, fmt.Errorf("scan observed final output: %w", err)
		}

		outputs = append(outputs, row)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read observed final outputs: %w", err)
	}

	return outputs, nil
}
