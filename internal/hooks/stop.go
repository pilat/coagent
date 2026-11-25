package hooks

import (
	"context"
	"strings"
)

func (h *Hooks) Stop(ctx context.Context, lastMessage string, active bool) (string, error) {
	payload := stopPayload{
		commonPayload:        h.commonPayload(stop),
		StopHookActive:       active,
		LastAssistantMessage: lastMessage,
	}
	var reasons []string

	for _, decision := range h.run(ctx, stop, nil, payload) {
		if decision.block {
			reason := decision.reason
			if reason == "" {
				reason = "Stop hook blocked completion: [" + decision.command + "]"
			}

			reasons = append(reasons, reason)
		}

		if decision.context != "" {
			reasons = append(reasons, decision.context)
		}
	}

	if ctx.Err() != nil {
		return "", ctx.Err()
	}

	return strings.Join(reasons, "\n\n"), nil
}
