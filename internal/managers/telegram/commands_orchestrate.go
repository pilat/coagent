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

		state := map[bool]string{true: "on.", false: "off. Use /orchestrate on to enable."}[on]

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
	texts := map[[2]bool]string{
		{true, true}:   "#%d is now an orchestrator: it can list, message and start your sessions.",
		{true, false}:  "#%d is already an orchestrator.",
		{false, true}:  "#%d is no longer an orchestrator.",
		{false, false}: "#%d is not an orchestrator.",
	}

	return fmt.Sprintf(texts[[2]bool{on, changed}], focus)
}
