package sessionstore

import (
	"context"
	"database/sql"
	"fmt"
)

func observerQuotesTx(ctx context.Context, tx *sql.Tx, work, observer, final int64, project string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT i.raw_content, i.source,
		COALESCE(json_extract(i.attributes, '$.schedule_id'), 0),
		json_type(i.attributes, '$.forwarded_by') IS NOT NULL FROM session_inbox i
		WHERE i.session_id = ? AND json_extract(i.attributes, '$.observer_session_id') = ?
		AND `+ObserverOpenInputSQL+` AND (? = 0 OR (i.state = 'accepted' AND i.accepted_message_id < ?))
		AND (i.source = 'schedule' OR (i.source = 'user' AND COALESCE(json_extract(i.attributes, '$.from'), '') <> 'assistant'))
		ORDER BY i.id`, work, observer, final, final)
	if err != nil {
		return nil, fmt.Errorf("load observed human input: %w", err)
	}
	defer rows.Close()
	var lines []string

	for rows.Next() {
		var content string
		var source InputSource

		var scheduleID int64
		var forwarded bool

		if err := rows.Scan(&content, &source, &scheduleID, &forwarded); err != nil {
			return nil, fmt.Errorf("scan observed human input: %w", err)
		}

		line := fmt.Sprintf("⏰ %s (%d) schedule #%d fired", project, work, scheduleID)

		if source == InputSourceUser {
			label := "user wrote"
			if forwarded {
				label = "you forwarded"
			}

			line = fmt.Sprintf("→ %s (%d) %s: %q", project, work, label, ClipMiddle(content, 2000))
		}

		lines = append(lines, line)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read observed human input: %w", err)
	}

	return lines, nil
}
