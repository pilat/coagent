package sessionstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"
)

const (
	ObserverSessionIDAttribute = "observer_session_id"
	ObservedSessionIDAttribute = "observed_session_id"
	ObservedKindAttribute      = "observed_kind"
	ObservedMessageIDAttribute = "observed_message_id"
)

// FinalMessageSQL identifies a work answer over messages aliased as m.
const FinalMessageSQL = `(m.role = 'assistant' AND COALESCE(m.tool_calls, '') IN ('', '[]', 'null')
		AND TRIM(COALESCE(m.content, ''), ' ' || char(9, 10, 13)) <> ''
		AND COALESCE(m.finish_type, '') IN ('', 'stop'))`

// ObserverOpenInputSQL selects open observer refs over session_inbox aliased as i.
const ObserverOpenInputSQL = `json_type(i.attributes, '$.observer_session_id') = 'integer' AND
(i.state = 'pending' OR (i.state = 'accepted' AND i.accepted_message_id > COALESCE((
 SELECT MAX(json_extract(a.attributes, '$.observed_message_id')) FROM session_inbox a
 WHERE a.session_id = json_extract(i.attributes, '$.observer_session_id') AND a.source = 'agent'
 AND json_extract(a.attributes, '$.observed_session_id') = i.session_id
 AND json_extract(a.attributes, '$.observed_kind') = 'answered' AND a.state <> 'cancelled'
), 0)))`

type observerWindow struct{ observer, lower int64 }

// ClipMiddle keeps the head and tail so the end of a long text stays visible.
func ClipMiddle(text string, limit int) string {
	chars := []rune(text)
	if len(chars) <= limit {
		return text
	}

	const marker = "\n[…]\n"

	tail := max(0, limit/4)
	head := max(0, limit-tail-utf8.RuneCountInString(marker))

	return string(chars[:head]) + marker + string(chars[len(chars)-tail:])
}

func notifyObserverOutputTx(
	ctx context.Context,
	tx *sql.Tx,
	c Commit,
	output Output,
	out *OutputCommit,
	result *CommitResult,
) error {
	if output.Session != 0 && output.Session != c.SessionID {
		return nil
	}

	if output.Phase == "final" && !output.Yield && output.MessageRef >= 0 &&
		output.MessageRef < len(result.MessageIDs) {
		return notifyObserversTx(ctx, tx, c.SessionID, result.MessageIDs[output.MessageRef], out.OutputID, "")
	}

	status := c.State.Status
	if status == nil && !result.BudgetFired {
		status = c.Unfired.State.Status
	}

	if status != nil && *status == SessionStatusError && output.ReleasesInput && output.MessageRef == -1 {
		return notifyObserversTx(ctx, tx, c.SessionID, 0, out.OutputID, out.Content)
	}

	return nil
}

func notifyObserversTx(ctx context.Context, tx *sql.Tx, work, final, output int64, notice string) error {
	windows, err := observerWindowsTx(ctx, tx, work, final)
	if err != nil || len(windows) == 0 {
		return err
	}

	var project string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(p.name, '') FROM sessions s
LEFT JOIN projects p ON p.id = s.project_id WHERE s.id = ?`, work).Scan(&project); err != nil {
		return fmt.Errorf("load observed project: %w", err)
	}

	for _, window := range windows {
		kind, text := "failed", notice

		attrs := map[string]any{ObservedSessionIDAttribute: work, ObservedKindAttribute: kind}
		if final != 0 {
			answer, err := observerAnswerTx(ctx, tx, work, window.lower, final)
			if err != nil {
				return err
			}

			kind, text = "answered", fmt.Sprintf("%q", answer)
			attrs[ObservedKindAttribute], attrs[ObservedMessageIDAttribute] = kind, final
		}

		_, err := EnqueueTx(ctx, tx, Input{
			SessionID: window.observer, Source: InputSourceAgent,
			Content:    ClipMiddle(fmt.Sprintf("← %s (%d) %s: %s", project, work, kind, text), 20000),
			Attributes: attrs, DeliveryKey: fmt.Sprintf("observer:%d", output),
		})
		if err != nil && !errors.Is(err, ErrSessionNotAcceptingInput) {
			return fmt.Errorf("enqueue observer input: %w", err)
		}
	}

	return nil
}

func observerWindowsTx(ctx context.Context, tx *sql.Tx, work, final int64) ([]observerWindow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT json_extract(i.attributes, '$.observer_session_id'),
COALESCE(MIN(i.accepted_message_id), 0) FROM session_inbox i
WHERE i.session_id = ? AND `+ObserverOpenInputSQL+`
AND (? = 0 OR (i.state = 'accepted' AND i.accepted_message_id < ?))
GROUP BY json_extract(i.attributes, '$.observer_session_id')`, work, final, final)
	if err != nil {
		return nil, fmt.Errorf("load observer windows: %w", err)
	}
	defer rows.Close()
	var windows []observerWindow

	for rows.Next() {
		var window observerWindow
		if err := rows.Scan(&window.observer, &window.lower); err != nil {
			return nil, fmt.Errorf("scan observer window: %w", err)
		}

		windows = append(windows, window)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read observer windows: %w", err)
	}

	return windows, nil
}

func observerAnswerTx(ctx context.Context, tx *sql.Tx, work, lower, final int64) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT COALESCE(m.content, '') FROM messages m
WHERE m.session_id = ? AND m.id > ? AND m.id <= ?
AND m.rejected_reason IS NULL AND m.compacted_at IS NULL AND `+FinalMessageSQL+`
ORDER BY m.position IS NULL, m.position, m.id`, work, lower, final)
	if err != nil {
		return "", fmt.Errorf("load observer answer: %w", err)
	}
	defer rows.Close()
	var finals []string

	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return "", fmt.Errorf("scan observer answer: %w", err)
		}

		if text := strings.TrimSpace(content); !slices.Contains(finals, text) {
			finals = append(finals, text)
		}
	}

	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("read observer answer: %w", err)
	}

	return strings.Join(finals, "\n\n---\n\n"), nil
}
