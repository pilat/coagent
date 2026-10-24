package telegram

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/pilat/coagent/internal/controllerapi"
)

func (m *Manager) ensureAssistantRoot(ctx context.Context) error {
	root, err := m.controller.EnsureAssistantRoot(ctx)
	if err != nil {
		return fmt.Errorf("ensure assistant session: %w", err)
	}

	topic, exists := topicIDFromAttributes(root.Attributes)
	if !exists || topic <= 0 {
		topic, err = m.createForumTopic(ctx, m.cfg.AssistantTopicName, m.cfg.SessionTopicIconEmojiID)
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

func (m *Manager) isAssistantRoot(id int64) bool {
	return id > 0 && id == m.assistantRootID
}

func (m *Manager) handleAssistantCommand(ctx context.Context, topic int64, text string) bool {
	command, _, _ := strings.Cut(text, " ")
	switch command {
	case commandKill, commandStop, commandClear, "/compact":
		_, _ = m.sendMessage(ctx, "This command is unavailable in the Assistant topic.", nil, topic)
		return true
	default:
		return false
	}
}
