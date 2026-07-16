package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

// PendingToolCall identifies one exact suspended tool invocation.
type PendingToolCall struct {
	ID   string
	Name string
}

// CallResolution describes the durable outcome of resolving a pending call.
type CallResolution uint8

const (
	CallResolutionInserted CallResolution = iota + 1
	CallResolutionAlreadyPresent
)

// PendingExternalCalls is deliberately global over the active transcript.
// External work is causal state: a later user or synthetic event cannot
// supersede it merely by becoming the latest turn.
func (s *svc) PendingExternalCalls() []PendingToolCall {
	calls := unresolvedCallsMatching(s.ms.getMessages(), func(tc llmwire.ToolCall) bool {
		return s.stagedCalls[tc.ID] != ""
	})

	result := make([]PendingToolCall, 0, len(calls))

	for _, call := range calls {
		result = append(result, PendingToolCall{ID: call.ID, Name: s.stagedCalls[call.ID]})
	}

	return result
}

func (s *svc) HasPendingExternalCall() bool {
	return len(s.PendingExternalCalls()) > 0
}

func (s *svc) ResolvePendingCall(
	ctx context.Context,
	call PendingToolCall,
	content string,
) (CallResolution, error) {
	if call.ID == "" || call.Name == "" {
		return 0, errors.New("resolve pending call: id and tool name are required")
	}

	messages := s.ms.getMessages()
	status := findToolCall(messages, call.ID)

	if status.duplicate {
		return 0, fmt.Errorf("resolve pending call %q: duplicate tool call id", call.ID)
	}

	if !status.found {
		return 0, fmt.Errorf("resolve pending call %q: tool call not found", call.ID)
	}

	if status.name != call.Name {
		return 0, fmt.Errorf(
			"resolve pending call %q: tool name mismatch: transcript=%q result=%q",
			call.ID,
			status.name,
			call.Name,
		)
	}

	ownerName, owned := s.stagedCalls[call.ID]
	if !owned || ownerName == "" {
		return 0, fmt.Errorf("resolve pending call %q: no external producer owns %q", call.ID, status.name)
	}

	if ownerName != call.Name {
		return 0, fmt.Errorf(
			"resolve pending call %q: producer name mismatch: ledger=%q result=%q",
			call.ID,
			ownerName,
			call.Name,
		)
	}

	if status.resolved {
		return CallResolutionAlreadyPresent, nil
	}

	if err := s.ms.addToolResult(ctx, call.ID, call.Name, content); err != nil {
		return 0, err
	}

	return CallResolutionInserted, nil
}

// HasPendingWork reports whether the current assistant turn has unresolved
// in-loop tools. External calls are excluded even if an erroneous newer turn
// exists; handlePreviousResult suspends on their global ledger first.
func (s *svc) HasPendingWork() bool {
	external := make(map[string]bool)

	for _, call := range s.PendingExternalCalls() {
		external[call.ID] = true
	}

	for id := range unresolvedToolCalls(s.ms.getMessages()) {
		if !external[id] {
			return true
		}
	}

	return false
}

func (s *svc) pendingExternalCallIDs() map[string]bool {
	calls := unresolvedCallsMatching(s.ms.getMessages(), func(tc llmwire.ToolCall) bool {
		return tool.IsExternalCall(tc.Name) || s.stagedCalls[tc.ID] != ""
	})

	out := make(map[string]bool, len(calls))

	for _, call := range calls {
		out[call.ID] = true
	}

	return out
}

func unresolvedCallsMatching(
	messages []llmwire.Message,
	match func(llmwire.ToolCall) bool,
) []llmwire.ToolCall {
	resolved := make(map[string]bool)

	for _, message := range messages {
		if message.Role == llmwire.RoleTool && message.ToolCallID != "" {
			resolved[message.ToolCallID] = true
		}
	}

	var result []llmwire.ToolCall

	for _, message := range messages {
		if message.Role != llmwire.RoleAssistant {
			continue
		}

		for _, tc := range message.ToolCalls {
			if tc.ID != "" && !resolved[tc.ID] && match(tc) {
				result = append(result, tc)
			}
		}
	}

	return result
}

type toolCallStatus struct {
	name      string
	found     bool
	resolved  bool
	duplicate bool
}

func findToolCall(messages []llmwire.Message, callID string) toolCallStatus {
	var status toolCallStatus

	for _, message := range messages {
		if message.Role == llmwire.RoleAssistant {
			for _, tc := range message.ToolCalls {
				if tc.ID != callID {
					continue
				}

				if status.found {
					status.duplicate = true
				}

				status.name = tc.Name
				status.found = true
			}
		}

		if message.Role == llmwire.RoleTool && message.ToolCallID == callID {
			status.resolved = true
		}
	}

	return status
}
