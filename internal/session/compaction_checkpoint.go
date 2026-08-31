package session

import (
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
)

// compactionHeaderSize keeps the existing immutable-header rule: an opening
// system/AGENTS row reserves itself plus the opening task when present,
// otherwise only the first row is header.
func compactionHeaderSize(messages []llmwire.Message) int {
	if len(messages) == 0 {
		return 0
	}

	if messages[0].Role == llmwire.RoleSystem || strings.HasPrefix(messages[0].Content, agentsMDMessagePrefix) {
		return min(2, len(messages))
	}

	return 1
}

// validateCompactionHeader rejects a header carrying tool protocol fields: the
// header is never summarized and a tool row there could never pair.
func validateCompactionHeader(header []llmwire.Message) error {
	for _, msg := range header {
		if msg.Role == llmwire.RoleTool || msg.ToolCallID != "" || len(msg.ToolCalls) > 0 {
			return fmt.Errorf("compaction header row %q carries tool protocol fields", msg.Role)
		}
	}

	return nil
}

// headerFitsLocked reports whether compaction can converge at all: the header is
// never summarized and the system prompt rides along on every request.
func (s *svc) headerFitsLocked(headerSize int) bool {
	size := estimateTokens(s.ms.messages[:headerSize]) + estimateText(s.prompt.systemPrompt())

	return size <= compactionCutoff(s.contextWindow())
}

// canonicalHead builds the canonical projection of the raw head: the ordinary
// repaired model projection, minus the current skill's activation when that
// activation falls inside the head (it is reattached verbatim instead).
func canonicalHead(messages []llmwire.Message, cp checkpointPrefix, split int) []llmwire.Message {
	head := sanitizeIncompleteCalls(repairTranscript(messages[cp.rawStart:split]))

	candIdx, candEnvelope := selectCurrentSkill(messages, compactionHeaderSize(messages), cp.summaryRowIdx)
	if candEnvelope == "" || candIdx < cp.rawStart || candIdx >= split {
		return head
	}

	return removeSkillActivation(head, messages[candIdx])
}

// sanitizeIncompleteCalls strips tool calls with no id or name: an aborted
// assistant turn is history, not a pairable call, and would otherwise fail
// provider validation forever.
func sanitizeIncompleteCalls(head []llmwire.Message) []llmwire.Message {
	out := make([]llmwire.Message, 0, len(head))

	for _, msg := range head {
		if msg.Role != llmwire.RoleAssistant {
			out = append(out, msg)

			continue
		}

		complete := make([]llmwire.ToolCall, 0, len(msg.ToolCalls))

		for _, tc := range msg.ToolCalls {
			if tc.ID != "" && tc.Name != "" {
				complete = append(complete, tc)
			}
		}

		if len(complete) == len(msg.ToolCalls) {
			out = append(out, msg)

			continue
		}

		if len(complete) == 0 && strings.TrimSpace(msg.Content) == "" {
			continue // the row carried only the aborted calls
		}

		msg.ToolCalls = complete
		out = append(out, msg)
	}

	return out
}

// removeSkillActivation drops one current-skill activation from the canonical
// head so the envelope can be reattached verbatim outside the summary. A mixed
// assistant response keeps its remaining calls as valid pairs.
func removeSkillActivation(head []llmwire.Message, cand llmwire.Message) []llmwire.Message {
	if cand.Role == llmwire.RoleUser {
		return dropUserEnvelopeRows(head, cand.Content)
	}

	if cand.Role != llmwire.RoleTool || cand.ToolCallID == "" {
		return head
	}

	out := make([]llmwire.Message, 0, len(head))

	for _, msg := range head {
		switch {
		case msg.Role == llmwire.RoleTool && msg.ToolCallID == cand.ToolCallID:
			continue
		case msg.Role == llmwire.RoleAssistant && hasToolCall(msg.ToolCalls, cand.ToolCallID):
			remaining := make([]llmwire.ToolCall, 0, len(msg.ToolCalls))

			for _, tc := range msg.ToolCalls {
				if tc.ID != cand.ToolCallID {
					remaining = append(remaining, tc)
				}
			}

			if len(remaining) == 0 && strings.TrimSpace(msg.Content) == "" {
				continue // the assistant row carried only this call
			}

			msg.ToolCalls = remaining
		}

		out = append(out, msg)
	}

	return out
}

func hasToolCall(calls []llmwire.ToolCall, id string) bool {
	for _, tc := range calls {
		if tc.ID == id {
			return true
		}
	}

	return false
}

// dropUserEnvelopeRows removes user rows carrying exactly the envelope text.
func dropUserEnvelopeRows(head []llmwire.Message, content string) []llmwire.Message {
	out := make([]llmwire.Message, 0, len(head))

	for _, msg := range head {
		if msg.Role == llmwire.RoleUser && msg.Content == content {
			continue
		}

		out = append(out, msg)
	}

	return out
}

// assembleCheckpointLocked builds the positioned replacement projection —
// header, marked summary, optional current skill, exact selected tail — plus
// the DBIDs of every row the checkpoint marks compacted. A carried reattachment
// row keeps its DBID and is only repositioned.
func (s *svc) assembleCheckpointLocked(
	cp checkpointPrefix,
	headerSize, split, candIdx int,
	candEnvelope string,
	summaryMsg llmwire.Message,
) ([]llmwire.Message, []int64, []int64) {
	messages := s.ms.messages
	rowIDs := s.ms.rowIDs

	carriedSkill := candIdx >= 0 && candIdx == cp.skillRowIdx

	var skillRow *llmwire.Message

	switch {
	case carriedSkill:
		skillRow = &messages[cp.skillRowIdx]
	case candEnvelope != "" && candIdx >= cp.rawStart && candIdx < split:
		skillRow = &llmwire.Message{Role: llmwire.RoleUser, Content: candEnvelope}
	}

	newMessages := make([]llmwire.Message, 0, headerSize+2+len(messages)-split)
	newRowIDs := make([]int64, 0, cap(newMessages))
	newMessages = append(newMessages, messages[:headerSize]...)
	newRowIDs = append(newRowIDs, rowIDs[:headerSize]...)
	newMessages = append(newMessages, summaryMsg)
	newRowIDs = append(newRowIDs, 0)

	if skillRow != nil {
		newMessages = append(newMessages, *skillRow)
		if carriedSkill {
			newRowIDs = append(newRowIDs, rowIDs[cp.skillRowIdx])
		} else {
			newRowIDs = append(newRowIDs, 0)
		}
	}

	newMessages = append(newMessages, messages[split:]...)
	newRowIDs = append(newRowIDs, rowIDs[split:]...)

	compactedIDs := make([]int64, 0, split-headerSize)

	for i := headerSize; i < split; i++ {
		if carriedSkill && i == cp.skillRowIdx {
			continue
		}

		if rowIDs[i] != 0 {
			compactedIDs = append(compactedIDs, rowIDs[i])
		}
	}

	return newMessages, newRowIDs, compactedIDs
}
