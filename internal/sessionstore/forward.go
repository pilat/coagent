package sessionstore

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

const (
	ForwardedByAttribute      = "forwarded_by"
	ForwardedInputAttribute   = "forwarded_input_id"
	ForwardedSessionAttribute = "forwarded_session_id"
)

// ForwardTx commits the user's exact words and their routing receipt atomically.
func ForwardTx(
	ctx context.Context,
	tx *sql.Tx,
	by int64,
	target int64,
	attributes map[string]any,
) (*Enqueued, error) {
	current, err := activeHumanInput(ctx, tx, by)
	if err != nil {
		return nil, err
	}

	if current == nil {
		return nil, ErrForwardInput
	}

	if strings.HasPrefix(strings.TrimSpace(current.RawContent), "/") {
		return nil, ErrForwardedCommand
	}

	attrs := cloneAttributes(attributes)
	attrs[ForwardedByAttribute], attrs[ForwardedInputAttribute] = by, current.ID

	result, err := EnqueueTx(ctx, tx, Input{
		SessionID: target, Source: InputSourceUser, Content: current.RawContent,
		Attributes: attrs, DeliveryKey: fmt.Sprintf("forward:%d", current.ID),
	})
	if err != nil || !result.Applied {
		return result, err
	}

	var agentType, project, title string
	if err := tx.QueryRowContext(ctx, `SELECT s.agent_type, p.name,
		COALESCE(json_extract(s.attributes, '$.title'), '') FROM sessions s
		JOIN projects p ON p.id = s.project_id WHERE s.id = ?`, target).
		Scan(&agentType, &project, &title); err != nil {
		return nil, fmt.Errorf("load forwarded session name: %w", err)
	}

	_, err = insertOutputTx(ctx, tx, OutputDraft{
		SessionID: by, Type: OutputMessage,
		Content:    fmt.Sprintf("→ #%d %s (forwarded)", target, chatDisplayName(agentType, project, title)),
		SourceKey:  fmt.Sprintf("forward:%d:%d", current.ID, target),
		Attributes: map[string]any{ForwardedSessionAttribute: target}, CreatedAt: result.Input.ReceivedAt,
	}, CommitLoop)
	if err != nil {
		return nil, err
	}

	return result, nil
}
