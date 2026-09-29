package sessioncalls

import (
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

// LatestCallRef identifies the newest persisted invocation matching a wire ID.
func LatestCallRef(
	messages []llmwire.Message,
	rowIDs []int64,
	callID, toolName string,
) (sessionstore.CallRef, error) {
	if len(messages) != len(rowIDs) || callID == "" || toolName == "" {
		return sessionstore.CallRef{}, errors.New("tool call reference requires aligned rows and identity")
	}

	for i := len(messages) - 1; i >= 0; i-- {
		message := messages[i]
		if message.Role != llmwire.RoleAssistant {
			continue
		}
		for index := len(message.ToolCalls) - 1; index >= 0; index-- {
			call := message.ToolCalls[index]
			if call.ID != callID {
				continue
			}
			if call.Name != toolName || rowIDs[i] <= 0 {
				return sessionstore.CallRef{}, fmt.Errorf("tool call %q has no matching durable owner", callID)
			}
			return sessionstore.CallRef{AssistantMessageID: rowIDs[i], Index: index}, nil
		}
	}
	return sessionstore.CallRef{}, fmt.Errorf("tool call %q has no assistant owner", callID)
}
