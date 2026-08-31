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

// tailLimit stages the tail constraints: the token floor with both low-water
// ceilings, then the ceilings alone (D2 — the byte ceiling beats the token
// floor), then nothing but legality (D3 — the tail is never empty, even when
// a single legal group breaches the ceiling).
type tailLimit struct {
	floor    bool
	ceilings bool
}

// tailLevels is the staged relaxation order every split search walks.
func tailLevels() []tailLimit {
	return []tailLimit{{floor: true, ceilings: true}, {ceilings: true}, {}}
}

// minTailTokens is the token floor the tail should meet: a tenth of the window,
// degrading to half the raw history estimate when less history exists (D3).
func minTailTokens(messages []llmwire.Message, base, window int) int {
	return min(window/10, estimateTokens(messages[base:])/2)
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

	minTail := minTailTokens(messages, base, window)

	for _, limit := range tailLevels() {
		if split, ok := selectTailSplit(messages, base, minTail, requestBaseEstimate, window, limit); ok {
			return split, true
		}
	}

	return 0, false
}

// selectTailSplit scans from the largest head down, returning the first
// candidate whose tail satisfies the staged limits, is a legal raw cut, and
// whose head fits the summarizer window. The loop starts at len-1, not len:
// the split never summarizes the whole raw range.
func selectTailSplit(
	messages []llmwire.Message,
	base, minTail, requestBaseEstimate, window int,
	limit tailLimit,
) (int, bool) {
	for split := len(messages) - 1; split > base; split-- {
		if limit.floor && estimateTokens(messages[split:]) < minTail {
			continue
		}

		if limit.ceilings {
			if totalBytes, count := imagePressure(
				messages[split:],
			); totalBytes > imageBytesLowWater ||
				count > imageCountLowWater {
				continue
			}
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
