package daemon

import (
	"github.com/pilat/coagent/internal/llmwire"
)

func transcriptOf(h *subagentHarness, sessionID int64) []llmwire.Message {
	messages, err := h.sessStore.LoadActiveMessages(h.ctx, sessionID)
	if err != nil {
		h.t.Fatalf("load transcript for session %d: %v", sessionID, err)
	}
	return toDTO(messages)
}
