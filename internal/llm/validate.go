package llm

import (
	"fmt"

	"github.com/pilat/coagent/internal/llmwire"
)

// ValidateToolPairing enforces the tool_use/tool_result pairing invariant that
// convertMessages relies on when building provider requests. A transcript that
// fails this would produce a provider 400 (unmatched tool_use / orphaned
// tool_result). It is the test oracle for transcript validity and may be used
// as a defensive assert before a Chat call.
//
// Rules:
//   - every assistant tool_call has a non-empty id;
//   - every tool result with an id references some assistant tool_call;
//   - every assistant tool_call has a matching tool result (no pending tool_use).
func ValidateToolPairing(messages []llmwire.Message) error {
	toolCallIDs := make(map[string]bool)

	for _, m := range messages {
		if m.Role != llmwire.RoleAssistant {
			continue
		}

		for _, tc := range m.ToolCalls {
			if tc.ID == "" {
				return fmt.Errorf("assistant tool_call with empty id (name=%q)", tc.Name)
			}

			toolCallIDs[tc.ID] = true
		}
	}

	resolved := make(map[string]bool)

	for _, m := range messages {
		if m.Role != llmwire.RoleTool || m.ToolCallID == "" {
			continue
		}

		if !toolCallIDs[m.ToolCallID] {
			return fmt.Errorf("orphaned tool_result for unknown tool_call id %q", m.ToolCallID)
		}

		resolved[m.ToolCallID] = true
	}

	for id := range toolCallIDs {
		if !resolved[id] {
			return fmt.Errorf("unresolved tool_call id %q has no tool_result", id)
		}
	}

	return nil
}
