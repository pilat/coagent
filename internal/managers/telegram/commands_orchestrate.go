package telegram

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

func (m *Manager) handleOrchestrate(ctx context.Context, focus int64, arg string) {
	arg = strings.ToLower(strings.TrimSpace(arg))
	chatID := strconv.FormatInt(m.effectiveChatID(), 10)
	var text string
	var err error

	switch arg {
	case "":
		var on bool
		on, err = m.chat.Orchestrator(ctx, chatID, focus)

		state := "off. Use /orchestrate on to enable."
		if on {
			state = "on."
		}

		text = fmt.Sprintf("#%d: orchestrator %s", focus, state)
	case "on", "off":
		var changed bool
		changed, err = m.chat.SetOrchestrator(ctx, chatID, focus, arg == "on")
		text = orchestrateChangeText(focus, arg == "on", changed)
	default:
		text = "Usage: /orchestrate on|off."
	}

	if err != nil {
		text = err.Error()
	}

	m.reply(ctx, "/orchestrate", strings.ReplaceAll(text, "\n", " "))
}

func orchestrateChangeText(focus int64, on, changed bool) string {
	switch {
	case on && changed:
		return fmt.Sprintf("#%d is now an orchestrator: it can list, message and start your sessions.", focus)
	case on:
		return fmt.Sprintf("#%d is already an orchestrator.", focus)
	case changed:
		return fmt.Sprintf("#%d is no longer an orchestrator.", focus)
	default:
		return fmt.Sprintf("#%d is not an orchestrator.", focus)
	}
}
