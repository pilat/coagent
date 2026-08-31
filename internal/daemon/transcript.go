package daemon

import (
	"slices"

	"github.com/pilat/coagent/internal/transcript"
)

// lastStoredAssistantText returns the content of the last text-only assistant
// message (backwards scan) — the child's displayable result text, or "".
func lastStoredAssistantText(msgs []*transcript.Message) string {
	for _, v := range slices.Backward(msgs) {
		m := v
		if m.Role == "assistant" && len(m.ToolCalls) == 0 && m.Content != "" {
			return m.Content
		}
	}

	return ""
}

// lastStoredMessageIsFinalAnswer reports whether the LAST assistant turn is
// text-only with content (strict last-message semantics, mirroring
// run.go:lastAssistantTextOnly) — the signal that the child produced a real final
// answer rather than stopping on a tool call. Trailing tool results are skipped;
// the first assistant/user message from the end decides.
func lastStoredMessageIsFinalAnswer(msgs []*transcript.Message) bool {
	for _, v := range slices.Backward(msgs) {
		switch v.Role {
		case "assistant":
			return len(v.ToolCalls) == 0 && v.Content != ""
		case "user":
			return false
		}
	}

	return false
}
