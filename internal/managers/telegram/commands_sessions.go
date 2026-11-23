package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/pilat/coagent/api/coagent/v1"
)

func (m *Manager) handleSessions(ctx context.Context, word string) {
	sessions, err := m.api.sessions.ListSessions(ctx, &coagentv1.ListSessionsRequest{})
	if err != nil {
		m.reply(ctx, "/sessions", strings.ReplaceAll(err.Error(), "\n", " "))
		return
	}

	focus, err := m.api.chats.GetChatFocus(
		ctx,
		&coagentv1.GetChatFocusRequest{ChatId: strconv.FormatInt(m.effectiveChatID(), 10)},
	)
	if err != nil {
		m.reply(ctx, "/sessions", strings.ReplaceAll(err.Error(), "\n", " "))
		return
	}

	filtered := filterSessionNames(sessions.GetSessions(), word)
	if len(filtered) == 0 {
		message := "No sessions. /new or /spawn."
		if word != "" {
			message = "No sessions match \"" + word + "\"."
		}

		m.reply(ctx, "/sessions", message)

		return
	}

	lines := make([]string, 0, min(len(filtered), sessionListLimit)+1)
	for _, session := range filtered[:min(len(filtered), sessionListLimit)] {
		line := fmt.Sprintf("#%d %s", session.GetId(), session.GetName())
		if session.GetScheduled() {
			line += " ⏰"
		}

		if session.GetId() == focus.GetSessionId() {
			line += " ◀"
		}

		lines = append(lines, line)
	}

	if remaining := len(filtered) - sessionListLimit; remaining > 0 {
		lines = append(lines, fmt.Sprintf("… and %d more", remaining))
	}

	m.reply(ctx, "/sessions", strings.Join(lines, "\n"))
}

func filterSessionNames(sessions []*coagentv1.SessionSummary, word string) []*coagentv1.SessionSummary {
	if word == "" {
		return sessions
	}

	filtered := make([]*coagentv1.SessionSummary, 0, len(sessions))

	word = strings.ToLower(word)
	for _, session := range sessions {
		if strings.Contains(strings.ToLower(session.GetName()), word) {
			filtered = append(filtered, session)
		}
	}

	return filtered
}
