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
	return m.preflightWithRetry(ctx, defaultStartupRetryPolicy())
}

func (m *Manager) preflightWithRetry(ctx context.Context, policy startupRetryPolicy) error {
	timeout := policy.timeout
	if timeout <= 0 {
		timeout = startupRetryTimeout
	}

	preflightCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	me, err := m.getMeWithStartupRetry(preflightCtx, policy)
	if err != nil {
		return err
	}

	if me.ID <= 0 {
		return errors.New("getMe returned an invalid bot user id")
	}

	m.botUserID = me.ID

	chat, err := m.getChatWithStartupRetry(preflightCtx, policy)
	if err != nil {
		if isChatNotFound(err) && m.target.topology == forumTopologyBot {
			return errors.New("private chat not found; open the bot and send /start first")
		}

		return fmt.Errorf("get forum chat: %w", err)
	}

	if m.target.topology == forumTopologyBot {
		return validateBotForumTarget(me, chat)
	}

	if err := validateGroupForumChat(chat); err != nil {
		return err
	}

	member, err := m.getChatMemberWithStartupRetry(preflightCtx, policy)
	if err != nil {
		return fmt.Errorf("get bot chat membership: %w", err)
	}

	return validateGroupForumMember(member)
}

func (m *Manager) getMeWithStartupRetry(ctx context.Context, policy startupRetryPolicy) (telegramBotUser, error) {
	var me telegramBotUser

	err := retryStartupCallWithContext(ctx, policy, func(callCtx context.Context) error {
		var err error
		me, err = m.getMe(callCtx)

		return err
	})
	if err != nil {
		return telegramBotUser{}, fmt.Errorf("get bot identity: %w", err)
	}

	return me, nil
}

func (m *Manager) getChatWithStartupRetry(ctx context.Context, policy startupRetryPolicy) (telegramForumChat, error) {
	var chat telegramForumChat

	err := retryStartupCallWithContext(ctx, policy, func(callCtx context.Context) error {
		var err error
		chat, err = m.getChat(callCtx, m.target.chatID)

		return err
	})

	return chat, err
}

func (m *Manager) getChatMemberWithStartupRetry(
	ctx context.Context,
	policy startupRetryPolicy,
) (telegramChatMember, error) {
	var member telegramChatMember

	err := retryStartupCallWithContext(ctx, policy, func(callCtx context.Context) error {
		var err error
		member, err = m.getChatMember(callCtx, m.target.chatID, m.botUserID)

		return err
	})

	return member, err
}

func validateBotForumTarget(me telegramBotUser, chat telegramForumChat) error {
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

func validateGroupForumChat(chat telegramForumChat) error {
	if chat.Type != "supergroup" {
		return fmt.Errorf("group forum target must be a supergroup, got %q", chat.Type)
	}

	if !chat.IsForum {
		return errors.New("group forum topics are disabled")
	}

	return nil
}

func validateGroupForumMember(member telegramChatMember) error {
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
