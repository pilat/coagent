package telegram

import (
	"context"
	"errors"
	"fmt"
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
	var apiErr *tgAPIError
	if errors.As(err, &apiErr) {
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
	if update.CallbackQuery != nil {
		m.handleCallback(ctx, update.CallbackQuery)
		return
	}

	log := logger.Ctx(ctx).Named("telegram")

	msg := update.Message
	if msg == nil {
		log.Debug("update_dropped", zap.String("reason", "not_a_message"))
		return
	}

	if msg.Text == "" && msg.Voice == nil {
		log.Debug("update_dropped", zap.String("reason", "no_text_or_voice"))
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

	m.processTopicUpdate(ctx, msg, log)
}

func (m *Manager) processTopicUpdate(ctx context.Context, msg *telegramMessage, log *zap.Logger) {
	threadID := msg.MessageThreadID
	if threadID == 0 {
		if m.target.topology == forumTopologyBot {
			_, _ = m.sendMessage(
				ctx,
				fmt.Sprintf("Open the “%s” topic to create or manage sessions.", m.cfg.ServiceTopicName),
				nil,
				0,
			)
		}

		log.Debug("update_dropped", zap.String("reason", "no_topic"))

		return
	}

	sessionID, hasSession := m.resolveSessionByTopicID(ctx, threadID)
	log.Info(
		"message_received",
		zap.Int64("thread_id", threadID),
		zap.Int64("session_id", sessionID),
		zap.Bool("has_session", hasSession),
		zap.String("text", textPreview(msg.Text)),
	)

	if msg.Voice != nil {
		m.handleVoiceMessage(ctx, msg, threadID, sessionID, hasSession)
		return
	}

	text := normalizeTextCommand(msg.Text)
	if threadID == m.serviceTopicID {
		m.handleServiceTopicMessage(ctx, text)
		return
	}

	if hasSession {
		m.handleSessionTopicMessage(ctx, sessionID, threadID, text)
		return
	}

	log.Info("update_dropped", zap.String("reason", "no_session_for_topic"), zap.Int64("thread_id", threadID))
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
