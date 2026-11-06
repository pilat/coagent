package sessionstore

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

type chatSessionActivity struct {
	ChatSession
	activity time.Time
}

// ChatSessions lists live owned roots by their last output or conversational input.
func (s *Store) ChatSessions(ctx context.Context, managerID string) ([]ChatSession, error) {
	rows, err := s.db.QueryContext(ctx, `WITH roots AS (
		SELECT s.id, p.name,
			COALESCE(json_extract(s.attributes, '$.title'), '') AS title FROM sessions s
		JOIN projects p ON p.id = s.project_id WHERE `+liveChatRootSQL+`
	)
	SELECT roots.id, roots.name, roots.title,
		s.created_at, output.created_at, input.received_at,
		EXISTS (SELECT 1 FROM schedules WHERE schedules.session_id = s.id AND NOT (`+SleepScheduleSQL+`))
	FROM roots
	JOIN sessions s ON s.id = roots.id
	LEFT JOIN session_outbox output ON output.id =
		(SELECT MAX(id) FROM session_outbox WHERE session_id = roots.id)
	LEFT JOIN session_inbox input ON input.id =
		(SELECT MAX(id) FROM session_inbox WHERE session_id = roots.id AND source IN ('user', 'agent'))`,
		managerID)
	if err != nil {
		return nil, fmt.Errorf("load chat session summaries: %w", err)
	}
	defer rows.Close()

	var active []chatSessionActivity

	for rows.Next() {
		session, err := scanChatSession(rows)
		if err != nil {
			return nil, err
		}

		active = append(active, session)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chat session summaries: %w", err)
	}

	slices.SortFunc(active, compareChatSessions)
	var sessions []ChatSession

	for _, session := range active {
		sessions = append(sessions, session.ChatSession)
	}

	return sessions, nil
}

func scanChatSession(rows *sql.Rows) (chatSessionActivity, error) {
	var session chatSessionActivity
	var created time.Time
	var output, input sql.NullTime
	var projectName, title string

	if err := rows.Scan(
		&session.ID,
		&projectName,
		&title,
		&created,
		&output,
		&input,
		&session.Scheduled,
	); err != nil {
		return chatSessionActivity{}, fmt.Errorf("scan chat session summary: %w", err)
	}

	session.Name = chatDisplayName(projectName, title)

	session.activity = output.Time
	if input.Time.After(session.activity) {
		session.activity = input.Time
	}

	if !output.Valid && !input.Valid {
		session.activity = created
	}

	return session, nil
}

func chatDisplayName(project, title string) string {
	if title != "" {
		return project + " · " + title
	}

	return project
}

func compareChatSessions(a, b chatSessionActivity) int {
	if order := b.activity.Compare(a.activity); order != 0 {
		return order
	}

	return cmp.Compare(b.ID, a.ID)
}
