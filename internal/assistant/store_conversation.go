package assistant

import (
	"context"
	"fmt"
	"slices"
)

func (s *Store) conversation(ctx context.Context, root int64) ([]Line, int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, at, kind, text, COUNT(*) OVER () FROM (
SELECT id, received_at AS at, 'user' AS kind, raw_content AS text
FROM session_inbox WHERE session_id = ? AND source = 'user' AND state = 'accepted'
UNION ALL
SELECT id, created_at AS at, 'say' AS kind, content AS text
FROM session_outbox WHERE session_id = ? AND type IN ('message_persistent', 'message_replaceable')
AND source_key LIKE 'message:%'
) ORDER BY at DESC, id DESC, kind DESC LIMIT ?`, root, root, conversationLines)
	if err != nil {
		return nil, 0, fmt.Errorf("query assistant conversation: %w", err)
	}
	defer rows.Close()
	var lines []Line
	total := 0

	for rows.Next() {
		var line Line
		if err := rows.Scan(&line.ID, &line.At, &line.Kind, &line.Text, &total); err != nil {
			return nil, 0, fmt.Errorf("scan assistant conversation: %w", err)
		}

		lines = append(lines, line)
	}

	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read assistant conversation: %w", err)
	}

	slices.Reverse(lines)

	return lines, total, nil
}
