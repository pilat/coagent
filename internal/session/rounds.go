package session

import "github.com/pilat/coagent/internal/llmwire"

// findMaskBoundary returns the exclusive start index of the last keepRounds complete
// rounds (assistant-with-tool-calls + results); everything before it may be masked.
func findMaskBoundary(msgs []llmwire.Message, keepRounds int) int {
	rounds := 0
	i := len(msgs) - 1

	for i >= 0 && rounds < keepRounds {
		// Skip tool result messages (they belong to the current round)
		for i >= 0 && msgs[i].Role == llmwire.RoleTool {
			i--
		}
		// We should now be at the assistant message with ToolCalls
		if i >= 0 && msgs[i].Role == llmwire.RoleAssistant && len(msgs[i].ToolCalls) > 0 {
			rounds++
			if rounds >= keepRounds {
				return i
			}

			i--
		} else if i >= 0 {
			// Not a round boundary — could be a steering user message, plain assistant,
			// or automated curator reminder injected between rounds. Skip over it.
			i--
		}
	}

	return 0
}
