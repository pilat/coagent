package telegram

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

func (m *Manager) resolveChatID() (int64, error) {
	if len(m.cfg.AllowedUserIDs) != 1 || m.cfg.AllowedUserIDs[0] <= 0 {
		return 0, errors.New("private chat requires exactly one positive allowed user id")
	}

	return m.cfg.AllowedUserIDs[0], nil
}

func (m *Manager) effectiveChatID() int64 {
	if m.chatID != 0 {
		return m.chatID
	}

	id, _ := m.resolveChatID()

	return id
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

	_, err = m.getChatWithStartupRetry(preflightCtx, policy)
	if err != nil {
		if isChatNotFound(err) {
			return errors.New("private chat not found; open the bot and send /start first")
		}

		return fmt.Errorf("get chat: %w", err)
	}

	return nil
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

func (m *Manager) getChatWithStartupRetry(ctx context.Context, policy startupRetryPolicy) (telegramChat, error) {
	var chat telegramChat

	err := retryStartupCallWithContext(ctx, policy, func(callCtx context.Context) error {
		var err error
		chat, err = m.getChat(callCtx, m.effectiveChatID())

		return err
	})

	return chat, err
}

func isChatNotFound(err error) bool {
	var apiErr *tgAPIError

	return errors.As(err, &apiErr) && apiErr.ErrorCode == http.StatusBadRequest &&
		strings.Contains(strings.ToLower(apiErr.Description), "chat not found")
}
