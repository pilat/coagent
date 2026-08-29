package session

import (
	"bytes"
	"fmt"
	"slices"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
)

// rawCutLegal requires the repaired projection field-equal to the raw suffix
// and pairing complete with unique call identities and result ownership: every
// tool group stays indivisible and every late result stays with its call.
func rawCutLegal(rawTail []llmwire.Message) bool {
	if len(rawTail) == 0 {
		return true
	}

	repaired := repairTranscript(rawTail)
	if len(repaired) != len(rawTail) {
		return false
	}

	for i := range repaired {
		if !rawMessagesEqual(repaired[i], rawTail[i]) {
			return false
		}
	}

	return validateRawGrouping(rawTail) == nil
}

// validateRawHead validates the repaired head with the same pairing rules the
// tail must already satisfy, after repair has applied the ordinary
// supersession/stub/reorder/duplicate policy.
func validateRawHead(repaired []llmwire.Message) error {
	if err := llm.ValidateToolPairing(repaired); err != nil {
		return fmt.Errorf("repaired head fails tool pairing: %w", err)
	}

	return validateRawGrouping(repaired)
}

// validateRawGrouping adds the session-scope checks beyond llm.ValidateToolPairing:
// unique non-empty assistant call ids, non-empty names and result ownership.
func validateRawGrouping(messages []llmwire.Message) error {
	callNames := make(map[string]string)

	for _, msg := range messages {
		if msg.Role != llmwire.RoleAssistant {
			continue
		}

		for _, tc := range msg.ToolCalls {
			if tc.ID == "" || tc.Name == "" {
				return fmt.Errorf("assistant tool_call missing id or name (name=%q)", tc.Name)
			}

			if _, dup := callNames[tc.ID]; dup {
				return fmt.Errorf("duplicate tool_call id %q across assistant rows", tc.ID)
			}

			callNames[tc.ID] = tc.Name
		}
	}

	seen := make(map[string]bool)

	for _, msg := range messages {
		if msg.Role != llmwire.RoleTool || msg.ToolCallID == "" {
			continue
		}

		if seen[msg.ToolCallID] {
			return fmt.Errorf("duplicate tool result for call id %q", msg.ToolCallID)
		}

		seen[msg.ToolCallID] = true

		if name := callNames[msg.ToolCallID]; msg.ToolName != "" && name != msg.ToolName {
			return fmt.Errorf("tool result for %q owned by %q, stored tool name %q", msg.ToolCallID, name, msg.ToolName)
		}
	}

	return nil
}

// summarizerBaseEstimateLocked estimates everything the summarizer request
// carries besides the canonical head: system prompt, instruction, focus, header
// reference and previous summary. Every byte participates in the 50% bound.
func (s *svc) summarizerBaseEstimateLocked(headerJSONL, prevSummary string) int {
	base := s.prompt.systemPrompt() +
		buildSummarizerPrompt(headerJSONL, prevSummary, "", s.focusSection()) +
		historySectionHeader()

	return estimateText(base)
}

// selectCheckpointSplit picks the maximal oldest raw prefix fitting half the
// window while the verbatim tail keeps the minimum tail estimate. The request
// estimate is not monotone in the split — a repair stub can exceed the real
// result it replaces — so every candidate is measured exactly.
func selectCheckpointSplit(
	messages []llmwire.Message,
	cp checkpointPrefix,
	requestBaseEstimate int,
	window int,
) (int, bool) {
	base := cp.rawStart
	if base >= len(messages) {
		return 0, false
	}

	minTail := window / 10
	if estimateTokens(messages[base:]) < minTail {
		minTail = 0 // not that much active history exists
	}

	for split := len(messages); split > base; split-- {
		if estimateTokens(messages[split:]) < minTail {
			continue
		}

		if !rawCutLegal(messages[split:]) {
			continue
		}

		serialized, err := serializeCanonical(repairTranscript(messages[base:split]))
		if err != nil || requestBaseEstimate+estimateText(serialized) > window/2 {
			continue
		}

		return split, true
	}

	return 0, false
}

// rawMessagesEqual compares the fields repair may touch, byte-for-byte.
func rawMessagesEqual(a, b llmwire.Message) bool {
	if a.Role != b.Role || a.Content != b.Content ||
		a.ToolCallID != b.ToolCallID || a.ToolName != b.ToolName ||
		a.ReasoningContent != b.ReasoningContent ||
		!bytes.Equal(a.ReasoningRaw, b.ReasoningRaw) ||
		a.CostUSD != b.CostUSD ||
		!slices.Equal(a.Images, b.Images) {
		return false
	}

	if len(a.ToolCalls) != len(b.ToolCalls) {
		return false
	}

	for i := range a.ToolCalls {
		if a.ToolCalls[i].ID != b.ToolCalls[i].ID || a.ToolCalls[i].Name != b.ToolCalls[i].Name ||
			!bytes.Equal(a.ToolCalls[i].Arguments, b.ToolCalls[i].Arguments) {
			return false
		}
	}

	return true
}
