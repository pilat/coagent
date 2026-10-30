package sessionstore

import (
	"context"
	"fmt"
)

// ChatSessions derives unread summaries from the log and puts the assistant first as #0.
func (s *Store) ChatSessions(ctx context.Context, managerID, chatID string) ([]ChatSession, error) {
	rows, err := s.db.QueryContext(ctx, `WITH roots AS (
		SELECT s.id, s.agent_type, s.updated_at, p.name,
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
		COALESCE(unread.latest_id, 0), COALESCE(latest.content, '')
	FROM roots JOIN unread ON unread.id = roots.id
	LEFT JOIN session_outbox latest ON latest.id = unread.latest_id
	ORDER BY roots.agent_type = 'assistant' DESC, roots.updated_at DESC, roots.id DESC`,
		managerID, managerID, chatID)
	if err != nil {
		return nil, fmt.Errorf("load chat session summaries: %w", err)
	}
	defer rows.Close()

	var sessions []ChatSession

	for rows.Next() {
		var session ChatSession

		var agentType, projectName, title string
		if err := rows.Scan(&session.SessionID, &agentType, &projectName, &title, &session.Unread,
			&session.LatestUnreadID, &session.LatestUnread); err != nil {
			return nil, fmt.Errorf("scan chat session summary: %w", err)
		}

		session.ID = session.SessionID

		session.Name = projectName
		if title != "" {
			session.Name += " · " + title
		}

		if agentType == AssistantAgentType {
			session.ID, session.Name = 0, "Assistant"
		}

		sessions = append(sessions, session)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate chat session summaries: %w", err)
	}

	return sessions, nil
}
