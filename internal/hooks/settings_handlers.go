package hooks

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

type eventHandlers struct {
	path   string
	event  string
	warned bool
	seen   map[handlerKey]string
}

func mergeSettings(ctx context.Context, files []settingsFile) map[string][]handler {
	loaded := make(map[string][]handler)
	seen := make(map[handlerKey]string)

	for _, file := range files {
		for event, groups := range file.Hooks {
			builder := eventHandlers{
				path:  file.Path,
				event: event,
				seen:  seen,
			}

			handlers := builder.load(ctx, groups)
			if len(handlers) == 0 {
				continue
			}

			loaded[event] = append(loaded[event], handlers...)
		}
	}

	return loaded
}

func hasHandlers(handlers map[string][]handler) bool {
	for _, entries := range handlers {
		if len(entries) > 0 {
			return true
		}
	}

	return false
}

func (b *eventHandlers) load(ctx context.Context, groups []matcherGroup) []handler {
	if b.event != preToolUse && b.event != postToolUse && b.event != stop {
		logger.Ctx(ctx).Debug("hooks_event_ignored", zap.String("path", b.path), zap.String("event", b.event))
		return nil
	}

	var handlers []handler
	for _, group := range groups {
		handlers = append(handlers, b.forGroup(ctx, group)...)
	}

	return handlers
}

func (b *eventHandlers) forGroup(ctx context.Context, group matcherGroup) []handler {
	match := compileMatcher(ctx, group.Matcher, b.path, b.event)
	var handlers []handler

	for _, config := range group.Hooks {
		h, ok := b.fromConfig(ctx, config)
		if !ok {
			continue
		}

		h.matcherText = group.Matcher
		h.match = match

		key := handlerKey{
			event:   b.event,
			matcher: h.matcherText,
			command: h.command,
			args:    fmt.Sprintf("%q", h.args),
		}
		if previous, exists := b.seen[key]; exists && previous != b.path {
			continue
		}

		b.seen[key] = b.path

		handlers = append(handlers, h)
	}

	return handlers
}

func (b *eventHandlers) fromConfig(ctx context.Context, config handlerConfig) (handler, bool) {
	if config.Type != "command" {
		if !b.warned {
			logger.Ctx(ctx).Warn("hooks_handler_unsupported", zap.String("path", b.path),
				zap.String("event", b.event), zap.String("type", config.Type))
			b.warned = true
		}

		return handler{}, false
	}

	if config.Command == "" || config.Shell == "powershell" {
		logger.Ctx(ctx).Warn("hooks_handler_unsupported", zap.String("path", b.path),
			zap.String("event", b.event), zap.String("command", config.Command))

		return handler{}, false
	}

	return parseHandler(ctx, b.event, config), true
}

func parseHandler(ctx context.Context, event string, config handlerConfig) handler {
	h := handler{
		command:   config.Command,
		execForm:  config.Args != nil,
		timeout:   config.Timeout,
		async:     config.Async || config.AsyncRewake,
		onFailure: config.OnFailure,
	}
	if config.Args != nil {
		h.args = *config.Args
	}

	if config.If != nil {
		logger.Ctx(ctx).Warn("hooks_if_ignored", zap.String("event", event), zap.String("command", h.command),
			zap.String("if", *config.If))
	}

	if h.async {
		logger.Ctx(ctx).Debug("hooks_async_inline", zap.String("event", event), zap.String("command", h.command))
	}

	return h
}
