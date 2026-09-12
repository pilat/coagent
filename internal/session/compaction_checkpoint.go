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
