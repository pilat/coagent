package telegram

import (
	"context"
	"fmt"
	"strings"

	"github.com/pilat/coagent/api/coagent/v1"
)

func (m *Manager) handleOrchestrate(ctx context.Context, focus int64, arg string) {
	arg = strings.ToLower(strings.TrimSpace(arg))
	var text string
	var err error

	switch arg {
	case "":
		var result *coagentv1.GetOrchestratorResponse
		result, err = m.api.sessions.GetOrchestrator(ctx, &coagentv1.GetOrchestratorRequest{SessionId: focus})

		state := "off. Use /orchestrate on to enable."
		if result.GetOn() {
			state = "on."
		}

		text = fmt.Sprintf("#%d: orchestrator %s", focus, state)
	case "on", "off":
		var result *coagentv1.SetOrchestratorResponse
		result, err = m.api.sessions.SetOrchestrator(
			ctx,
			&coagentv1.SetOrchestratorRequest{SessionId: focus, On: arg == "on"},
		)
		text = orchestrateChangeText(focus, arg == "on", result.GetChanged())
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
