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

// ChatSessions derives unread summaries from the log and puts the assistant first as #0.
func (s *Store) ChatSessions(ctx context.Context, managerID, chatID string) ([]ChatSession, error) {
	rows, err := s.db.QueryContext(ctx, `WITH roots AS (
		SELECT s.id, s.agent_type, p.name,
			COALESCE(json_extract(s.attributes, '$.title'), '') AS title FROM sessions s
		JOIN projects p ON p.id = s.project_id WHERE `+liveChatRootSQL+`
	), unread AS (
		SELECT roots.id, COUNT(o.id) AS unread, MAX(o.id) AS latest_id FROM roots
		LEFT JOIN chat_seen seen ON seen.manager_id = ? AND seen.chat_id = ? AND seen.session_id = roots.id
		LEFT JOIN session_outbox o ON o.session_id = roots.id AND o.id > COALESCE(seen.last_outbox_id, 0)
			AND o.type = 'message'
		GROUP BY roots.id
	)
	SELECT roots.id, roots.agent_type, roots.name, roots.title, unread.unread,
		COALESCE(unread.latest_id, 0), COALESCE(latest.content, ''),
		s.created_at, output.created_at, input.received_at,
		EXISTS (SELECT 1 FROM schedules WHERE schedules.session_id = s.id AND NOT (`+SleepScheduleSQL+`))
	FROM roots JOIN unread ON unread.id = roots.id
	JOIN sessions s ON s.id = roots.id
	LEFT JOIN session_outbox latest ON latest.id = unread.latest_id
	LEFT JOIN session_outbox output ON output.id =
		(SELECT MAX(id) FROM session_outbox WHERE session_id = roots.id)
	LEFT JOIN session_inbox input ON input.id =
		(SELECT MAX(id) FROM session_inbox WHERE session_id = roots.id AND source IN ('user', 'agent'))`,
		managerID, managerID, chatID)
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
	var agentType, projectName, title string

	if err := rows.Scan(&session.SessionID, &agentType, &projectName, &title, &session.Unread,
		&session.LatestUnreadID, &session.LatestUnread, &created, &output, &input, &session.Scheduled); err != nil {
		return chatSessionActivity{}, fmt.Errorf("scan chat session summary: %w", err)
	}

	session.ID = session.SessionID

	session.Name = chatDisplayName(agentType, projectName, title)
	if agentType == AssistantAgentType {
		session.ID = 0
	}

	session.activity = output.Time
	if input.Time.After(session.activity) {
		session.activity = input.Time
	}

	if !output.Valid && !input.Valid {
		session.activity = created
	}

	return session, nil
}

func chatDisplayName(agentType, project, title string) string {
	if agentType == AssistantAgentType {
		return "Assistant"
	}

	if title != "" {
		return project + " · " + title
	}

	return project
}

func compareChatSessions(a, b chatSessionActivity) int {
	if a.ID == 0 || b.ID == 0 {
		return cmp.Compare(a.ID, b.ID)
	}

	if (a.Unread > 0) != (b.Unread > 0) {
		if a.Unread > 0 {
			return -1
		}

		return 1
	}

	if order := b.activity.Compare(a.activity); order != 0 {
		return order
	}

	return cmp.Compare(b.ID, a.ID)
}
