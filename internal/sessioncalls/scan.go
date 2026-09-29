package sessioncalls

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/transcript"
)

// ErrAmbiguousCallID marks overlapping assistant calls with the same identity.
var ErrAmbiguousCallID = errors.New("ambiguous tool call id")

// Snapshot describes unresolved calls in active transcript order.
type Snapshot struct {
	GlobalUnresolved  []llmwire.ToolCall
	CurrentUnresolved map[string]string
	Calls             map[string]CallStatus
}

// CallStatus describes the latest invocation with a given raw call ID.
type CallStatus struct {
	Name     string
	Resolved bool
}

// Scan allows reuse only after the preceding invocation has a result.
func Scan(messages []llmwire.Message) (Snapshot, error) {
	result := Snapshot{Calls: make(map[string]CallStatus)}
	pending := make(map[string]int)
	resolved := make(map[int]bool)
	var calls []llmwire.ToolCall
	var turns []int
	latest := -1

	for i, message := range messages {
		if message.Role == llmwire.RoleAssistant {
			seen := make(map[string]bool)
			for _, call := range message.ToolCalls {
				if call.ID == "" {
					continue
				}

				_, exists := pending[call.ID]
				if seen[call.ID] || exists {
					return Snapshot{}, fmt.Errorf("%w: duplicate tool call id %q", ErrAmbiguousCallID, call.ID)
				}

				result.Calls[call.ID] = CallStatus{Name: call.Name}
				seen[call.ID] = true
				pending[call.ID] = len(calls)
				calls = append(calls, call)
				turns = append(turns, i)
			}
		}

		if message.Role == llmwire.RoleTool && message.ToolCallID != "" {
			if index, exists := pending[message.ToolCallID]; exists {
				resolved[index] = true
				status := result.Calls[message.ToolCallID]
				status.Resolved = true
				result.Calls[message.ToolCallID] = status
				delete(pending, message.ToolCallID)
			}
		}
	}

	for i, message := range slices.Backward(messages) {
		if message.Role == llmwire.RoleUser {
			break
		}

		if message.Role == llmwire.RoleAssistant {
			latest = i
			break
		}
	}

	for i, call := range calls {
		if resolved[i] {
			continue
		}
		result.GlobalUnresolved = append(result.GlobalUnresolved, call)
		if turns[i] == latest {
			if result.CurrentUnresolved == nil {
				result.CurrentUnresolved = make(map[string]string)
			}
			result.CurrentUnresolved[call.ID] = call.Name
		}
	}

	return result, nil
}

// ScanStored decodes durable call lists and applies the same identity rules.
func ScanStored(messages []*transcript.Message) (Snapshot, error) {
	wire, err := decodeStored(messages)
	if err != nil {
		return Snapshot{}, err
	}

	return Scan(wire)
}

func decodeStored(messages []*transcript.Message) ([]llmwire.Message, error) {
	wire := make([]llmwire.Message, len(messages))

	for i, message := range messages {
		if message == nil {
			return nil, fmt.Errorf("nil stored message at index %d", i)
		}

		wire[i] = llmwire.Message{Role: message.Role, ToolCallID: message.ToolCallID}
		if len(message.ToolCalls) > 0 {
			if err := json.Unmarshal(message.ToolCalls, &wire[i].ToolCalls); err != nil {
				return nil, fmt.Errorf("decode tool calls for message %d: %w", message.ID, err)
			}
		}
	}

	return wire, nil
}
