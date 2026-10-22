package telegram

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/pilat/coagent/internal/controllerapi"
)

const assistantAgentType = "assistant"

func (m *Manager) ensureAssistantRoot(ctx context.Context) error {
	root, err := m.controller.EnsureAssistantRoot(ctx)
	if err != nil {
		return fmt.Errorf("ensure assistant session: %w", err)
	}

	topic, exists := topicIDFromAttributes(root.Attributes)
	if !exists || topic <= 0 {
		topic, err = m.createForumTopic(ctx, m.assistantTopicName(), m.cfg.SessionTopicIconEmojiID)
		if err != nil {
			return fmt.Errorf("create assistant topic: %w", err)
		}

		attrs := maps.Clone(root.Attributes)
		if attrs == nil {
			attrs = make(map[string]any)
		}

		attrs["telegram_topic_id"] = topic
		if err := m.controller.SetSessionAttributes(ctx, controllerapi.SessionSetAttributesData{
			SessionID: root.SessionID, Attributes: attrs,
		}); err != nil {
			return fmt.Errorf("bind assistant topic: %w", err)
		}
	}

	m.assistantRootID = root.SessionID
	m.registerTopic(root.SessionID, topic)
	m.setWorkDir(root.SessionID, root.WorkDir)

	return nil
}

func (m *Manager) assistantTopicName() string {
	if m.cfg.AssistantTopicName != "" {
		return m.cfg.AssistantTopicName
	}

	return "Assistant"
}

func (m *Manager) isAssistantSession(ctx context.Context, id int64) bool {
	if id > 0 && id == m.assistantRootID {
		return true
	}

	sessions, err := m.controller.ListSessions(ctx)
	if err != nil {
		return false
	}

	for _, session := range sessions {
		if session.ID == id && session.AgentType == assistantAgentType {
			return true
		}
	}

	return false
}

func (m *Manager) handleAssistantCommand(ctx context.Context, id, topic int64, text string) bool {
	command, _, _ := strings.Cut(text, " ")
	switch command {
	case commandKill, commandStop, commandClear, "/compact":
		_, _ = m.sendMessage(ctx, "This command is unavailable in the Assistant topic.", nil, topic)
		return true
	default:
		return false
	}
}

func filterOutAssistantRoots(sessions []controllerapi.SessionInfo) []controllerapi.SessionInfo {
	kept := make([]controllerapi.SessionInfo, 0, len(sessions))
	for _, session := range sessions {
		if session.AgentType != assistantAgentType {
			kept = append(kept, session)
		}
	}

	return kept
}
