package telegram

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

func (m *Manager) pollLoop(ctx context.Context) {
	log := logger.Ctx(ctx).Named("telegram.poll")
	backoff := reconnectBackoffBase
	fatalWarned := false

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		updates, err := m.getUpdates(ctx, m.updateOffset)
		if err != nil {
			wait := nextPollWait(err, &backoff, &fatalWarned, log)
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
				continue
			}
		}

		backoff = reconnectBackoffBase
		fatalWarned = false

		for _, update := range updates {
			m.updateOffset = update.UpdateID + 1
			m.processUpdate(ctx, update)
		}
	}
}

func nextPollWait(err error, backoff *time.Duration, fatalWarned *bool, log *zap.Logger) time.Duration {
	if apiErr, ok := errors.AsType[*tgAPIError](err); ok {
		if apiErr.RetryAfter > 0 {
			log.Warn("getupdates_rate_limited", zap.Int("retry_after", apiErr.RetryAfter))
			return time.Duration(apiErr.RetryAfter) * time.Second
		}

		if isFatalPollError(apiErr.ErrorCode) {
			if !*fatalWarned {
				*fatalWarned = true

				log.Error(
					"getupdates_fatal",
					zap.Int("error_code", apiErr.ErrorCode),
					zap.String("description", apiErr.Description),
				)
			}

			*backoff = reconnectBackoffMax

			return *backoff
		}
	}

	log.Warn("getupdates_failed", zap.Duration("retry_in", *backoff), zap.Error(err))
	wait := *backoff
	*backoff = min(*backoff*2, reconnectBackoffMax)

	return wait
}

func isFatalPollError(code int) bool {
	return code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusConflict
}

func (m *Manager) processUpdate(ctx context.Context, update telegramUpdate) {
	log := logger.Ctx(ctx).Named("telegram")

	msg := update.Message
	if msg == nil {
		log.Debug("update_dropped", zap.String("reason", "not_a_message"))
		return
	}

	if msg.Text == "" && msg.Voice == nil && !hasAttachment(msg) {
		log.Debug("update_dropped", zap.String("reason", "no_text_or_voice_or_attachment"))
		return
	}

	if msg.Chat.ID != m.effectiveChatID() {
		log.Debug("update_dropped", zap.String("reason", "other_chat"), zap.Int64("chat_id", msg.Chat.ID))
		return
	}

	if msg.From == nil || !m.isAllowedUser(msg.From.ID) {
		log.Debug("update_dropped", zap.String("reason", "user_not_allowed"))
		return
	}

	log.Info("message_received", zap.String("text", textPreview(msg.Text)))
	m.processChatUpdate(ctx, msg)
}

func (m *Manager) processChatUpdate(ctx context.Context, msg *telegramMessage) {
	if msg.Voice == nil && !hasAttachment(msg) {
		m.handleChatText(ctx, normalizeTextCommand(msg.Text))
		return
	}

	focus, err := m.chat.ChatFocus(ctx, m.chatKey())
	if err != nil {
		m.reply(ctx, err.Error())
		return
	}

	sessionID, err := m.inputSessionID(ctx, focus)
	if err != nil {
		m.reply(ctx, err.Error())
		return
	}

	if msg.Voice != nil {
		m.handleVoiceMessage(ctx, msg, sessionID)
		return
	}

	if focus == 0 {
		if msg.Caption != "" {
			m.handleChatText(ctx, normalizeTextCommand(msg.Caption))
		}

		return
	}

	m.handleAttachment(ctx, msg, attachmentOf(msg), sessionID)
}

func textPreview(value string) string {
	const limit = 48

	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}

	return string(runes[:limit]) + "…"
}

func (m *Manager) isAllowedUser(userID int64) bool {
	return slices.Contains(m.cfg.AllowedUserIDs, userID)
}
