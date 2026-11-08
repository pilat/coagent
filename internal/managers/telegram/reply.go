package telegram

import (
	"context"
	"errors"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

func (m *Manager) reply(ctx context.Context, command, text string) {
	if _, err := m.sendMessage(ctx, text); err != nil {
		logReplyFailure(ctx, command, err)
	}
}

func (m *Manager) replyHTML(ctx context.Context, command, html string) {
	if _, err := m.sendRawHTML(ctx, html); err != nil {
		logReplyFailure(ctx, command, err)
	}
}

func logReplyFailure(ctx context.Context, command string, err error) {
	log := logger.Ctx(ctx).Named("telegram.reply")

	fields := []zap.Field{zap.Error(err)}
	if command != "" {
		fields = append(fields, zap.String("command", command))
	}

	if apiErr, ok := errors.AsType[*tgAPIError](err); ok {
		fields = append(fields, zap.Int("code", apiErr.ErrorCode), zap.String("description", apiErr.Description))
	}

	log.Warn("reply_failed", fields...)
}
