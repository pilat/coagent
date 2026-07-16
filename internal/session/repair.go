package session

import (
	"fmt"

	"github.com/pilat/coagent/internal/llmwire"
)

// repairTranscript ensures tool calls and results are properly paired.
// It performs four repairs matching OpenClaw's repairToolUseResultPairing:
//  1. Drops orphaned tool results (no matching assistant tool call)
//  2. Inserts synthetic error results for tool calls with no result
//  3. Reorders tool results to immediately follow their assistant message
//  4. Drops duplicate results for the same tool call ID
func repairTranscript(messages []llmwire.Message) []llmwire.Message {
	return repairTranscriptExcluding(messages, nil)
}

// repairTranscriptExcluding is repairTranscript that never fabricates a result
// for a tool_call in pendingCallIDs — those are genuinely-pending external calls
// (sleep, blocking task) awaiting an out-of-band outcome; stubbing them would
// corrupt the resume. The loop never reaches the LLM with such a call pending
// (handlePreviousResult returns first), so this is a defensive guard.
func repairTranscriptExcluding(messages []llmwire.Message, pendingCallIDs map[string]bool) []llmwire.Message {
	if len(messages) == 0 {
		return messages
	}

	allToolCallIDs := make(map[string]bool)

	for _, msg := range messages {
		if msg.Role == llmwire.RoleAssistant {
			for _, tc := range msg.ToolCalls {
				if tc.ID != "" {
					allToolCallIDs[tc.ID] = true
				}
			}
		}
	}

	// Index tool results by their ToolCallID for reordering
	resultsByCallID := make(map[string]llmwire.Message)
	seenResultIDs := make(map[string]bool)

	for _, msg := range messages {
		if msg.Role == llmwire.RoleTool && msg.ToolCallID != "" {
			if seenResultIDs[msg.ToolCallID] {
				continue // duplicate — keep first only
			}

			if allToolCallIDs[msg.ToolCallID] {
				resultsByCallID[msg.ToolCallID] = msg
				seenResultIDs[msg.ToolCallID] = true
			}
			// else: orphaned — will be dropped
		}
	}

	// Rebuild: walk messages, emit assistant + ordered results, skip bare tool messages
	result := make([]llmwire.Message, 0, len(messages))
	emittedResults := make(map[string]bool)

	for _, msg := range messages {
		switch {
		case msg.Role == llmwire.RoleAssistant && len(msg.ToolCalls) > 0:
			result = append(result, msg)
			emitToolResults(&result, msg.ToolCalls, resultsByCallID, emittedResults, pendingCallIDs)

		case msg.Role == llmwire.RoleTool && msg.ToolCallID != "":
			// Skip — already handled above (reordered or dropped)
			continue

		case msg.Role == llmwire.RoleTool && msg.ToolCallID == "":
			// Legacy tool results with no ID — preserve as-is
			result = append(result, msg)

		default:
			// User messages, plain assistant messages — keep as-is
			result = append(result, msg)
		}
	}

	return result
}

// emitToolResults appends ordered tool results (or synthetic error stubs) for an assistant message's tool calls.
func emitToolResults(
	result *[]llmwire.Message,
	toolCalls []llmwire.ToolCall,
	resultsByCallID map[string]llmwire.Message,
	emittedResults map[string]bool,
	pendingCallIDs map[string]bool,
) {
	incomplete := hasIncompleteToolCalls(toolCalls)

	for _, tc := range toolCalls {
		if tc.ID == "" || emittedResults[tc.ID] {
			continue
		}

		emittedResults[tc.ID] = true

		if tr, ok := resultsByCallID[tc.ID]; ok {
			*result = append(*result, tr)
			continue
		}

		// A genuinely-pending external call must not be stubbed — leave it
		// unmatched (the loop won't send this transcript while it's pending).
		if pendingCallIDs[tc.ID] {
			continue
		}

		if incomplete {
			continue
		}

		*result = append(*result, llmwire.Message{
			Role:       llmwire.RoleTool,
			ToolCallID: tc.ID,
			ToolName:   tc.Name,
			Content:    fmt.Sprintf("[transcript repair] missing tool result for %s (id: %s)", tc.Name, tc.ID),
		})
	}
}

// hasIncompleteToolCalls returns true if any tool call looks incomplete
// (missing ID or name), indicating the assistant message was likely
// aborted or errored mid-generation.
func hasIncompleteToolCalls(calls []llmwire.ToolCall) bool {
	for _, tc := range calls {
		if tc.ID == "" || tc.Name == "" {
			return true
		}
	}

	return false
}
