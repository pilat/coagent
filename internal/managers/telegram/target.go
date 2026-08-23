package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

type forumTopology string

const (
	forumTopologyGroup forumTopology = "group"
	forumTopologyBot   forumTopology = "bot"
)

type forumTarget struct {
	chatID   int64
	topology forumTopology
}

func (m *Manager) resolveForumTarget() (forumTarget, error) {
	if m.cfg.TargetChatID != nil {
		return forumTarget{chatID: *m.cfg.TargetChatID, topology: forumTopologyGroup}, nil
	}

	if len(m.cfg.AllowedUserIDs) != 1 || m.cfg.AllowedUserIDs[0] <= 0 {
		return forumTarget{}, errors.New("private bot forum requires exactly one positive allowed user id")
	}

	return forumTarget{chatID: m.cfg.AllowedUserIDs[0], topology: forumTopologyBot}, nil
}

func (m *Manager) effectiveChatID() int64 {
	if m.target.chatID != 0 {
		return m.target.chatID
	}

	if m.cfg.TargetChatID != nil {
		return *m.cfg.TargetChatID
	}

	return 0
}

func (m *Manager) preflight(ctx context.Context) error {
	me, err := m.getMe(ctx)
	if err != nil {
		return fmt.Errorf("get bot identity: %w", err)
	}

	if me.ID <= 0 {
		return errors.New("getMe returned an invalid bot user id")
	}

	m.botUserID = me.ID

	chat, err := m.getChat(ctx, m.target.chatID)
	if err != nil {
		if isChatNotFound(err) && m.target.topology == forumTopologyBot {
			return errors.New("private chat not found; open the bot and send /start first")
		}

		return fmt.Errorf("get forum chat: %w", err)
	}

	if m.target.topology == forumTopologyBot {
		if !me.HasTopicsEnabled {
			return errors.New("bot Threaded Mode is disabled; enable it in BotFather")
		}

		if me.AllowsUsersToCreateTopics {
			return errors.New(
				"bot allows users to create topics; enable BotFather's Disallow users to create new threads",
			)
		}

		if chat.Type != "private" {
			return fmt.Errorf("private bot forum target must be a private chat, got %q", chat.Type)
		}

		return nil
	}

	if chat.Type != "supergroup" {
		return fmt.Errorf("group forum target must be a supergroup, got %q", chat.Type)
	}

	if !chat.IsForum {
		return errors.New("group forum topics are disabled")
	}

	member, err := m.getChatMember(ctx, m.target.chatID, m.botUserID)
	if err != nil {
		return fmt.Errorf("get bot chat membership: %w", err)
	}

	if member.Status != "administrator" {
		return errors.New("bot is not a group administrator")
	}

	if !member.CanManageTopics {
		return errors.New("bot administrator is missing can_manage_topics")
	}

	if !member.CanDeleteMessages {
		return errors.New("bot administrator is missing can_delete_messages")
	}

	return nil
}

func isChatNotFound(err error) bool {
	var apiErr *tgAPIError

	return errors.As(err, &apiErr) && apiErr.ErrorCode == http.StatusBadRequest &&
		strings.Contains(strings.ToLower(apiErr.Description), "chat not found")
}
