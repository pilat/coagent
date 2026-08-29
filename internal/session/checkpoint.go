package session

import (
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

// Host-owned checkpoint wrapper. The complete wrapper is what makes a row a
// recognizable previous summary; delimiter collisions in user content are
// accepted user-controlled content, not a security boundary.
const (
	compactionMarkOpen  = "[CONTEXT SUMMARY of older history — lossy; later verbatim messages are newer and take precedence on conflict]"
	compactionMarkClose = "[/CONTEXT SUMMARY]"

	summarizeHeaderSection  = "IMMUTABLE HEADER REFERENCE"
	summarizePrevSection    = "PREVIOUS SUMMARY"
	summarizeHistorySection = "HISTORY TO SUMMARIZE"
)

// checkpointPrefix describes the scaffolding a previous successful compaction
// left between the immutable header and the raw transcript.
type checkpointPrefix struct {
	summaryRowIdx int    // previous marked summary row, -1 when none
	prevSummary   string // model-authored text extracted from that row
	skillRowIdx   int    // previous host reattachment row, -1 when none
	rawStart      int    // first raw (non-scaffolding) row after the header
}

// isMarkedSummary reports whether content carries the complete outer wrapper.
func isMarkedSummary(content string) bool {
	return strings.HasPrefix(content, compactionMarkOpen) && strings.HasSuffix(content, compactionMarkClose)
}

// parseMarkedSummary splits a marked summary row into its model-authored text
// and host-owned background section. Only the complete outer wrapper qualifies.
func parseMarkedSummary(content string) (string, string, bool) {
	if !isMarkedSummary(content) {
		return "", "", false
	}

	inner := content[len(compactionMarkOpen) : len(content)-len(compactionMarkClose)]
	inner = strings.TrimPrefix(inner, "\n\n")
	inner = strings.TrimSuffix(inner, "\n")

	if idx := strings.LastIndex(inner, backgroundSectionMarker); idx >= 0 {
		return strings.TrimSuffix(inner[:idx], "\n"), inner[idx:], true
	}

	return inner, "", true
}

// renderMarkedSummary wraps model text in the host marker, with the host-owned
// active-background section inside the wrapper when present.
func renderMarkedSummary(modelText, background string) string {
	out := compactionMarkOpen + "\n\n" + modelText
	if background != "" {
		out += "\n" + background
	}

	return out + compactionMarkClose
}

// lastEnvelope returns the last canonical skill envelope in content, if any.
// Transport stamping may prefix the content, so extraction is not exactness.
func lastEnvelope(content string) (string, bool) {
	envs := builtin.ExtractRenderedSkills(content)
	if len(envs) == 0 {
		return "", false
	}

	return envs[len(envs)-1].Envelope, true
}

// exactEnvelope reports whether content is exactly one canonical envelope —
// the shape host-authored reattachment rows have.
func exactEnvelope(content string) (string, bool) {
	env, ok := lastEnvelope(content)
	if !ok {
		return "", false
	}

	return env, strings.TrimSpace(content) == env
}

// parseCheckpointPrefix recognizes a previous compaction's scaffolding: the
// marked summary row after the header plus at most one exact skill reattachment
// right after it. Anything else is raw history.
func parseCheckpointPrefix(messages []llmwire.Message, headerSize int) checkpointPrefix {
	cp := checkpointPrefix{summaryRowIdx: -1, skillRowIdx: -1, rawStart: headerSize}

	if headerSize >= len(messages) {
		return cp
	}

	row := messages[headerSize]
	if row.Role != llmwire.RoleUser {
		return cp
	}

	modelText, _, ok := parseMarkedSummary(row.Content)
	if !ok {
		return cp
	}

	cp.summaryRowIdx = headerSize
	cp.prevSummary = modelText
	cp.rawStart = headerSize + 1

	if cp.rawStart < len(messages) {
		next := messages[cp.rawStart]
		if _, isEnvelope := exactEnvelope(next.Content); isEnvelope && next.Role == llmwire.RoleUser {
			cp.skillRowIdx = cp.rawStart
			cp.rawStart++
		}
	}

	return cp
}

// selectCurrentSkill returns the latest skill candidate in position order:
// rendered envelopes in role-user rows or in paired role-tool rows named
// `skill`. skip excludes one row (the previous summary row — its model text may
// quote an envelope). Failed calls and batch-shaped output never qualify.
func selectCurrentSkill(messages []llmwire.Message, from, skip int) (int, string) {
	last, envelope := -1, ""

	for i := from; i < len(messages); i++ {
		if i == skip {
			continue
		}

		msg := messages[i]

		switch {
		case msg.Role == llmwire.RoleUser:
			if env, ok := lastEnvelope(msg.Content); ok {
				last, envelope = i, env
			}
		case msg.Role == llmwire.RoleTool && msg.ToolName == tool.IDSkill:
			if env, ok := lastEnvelope(msg.Content); ok {
				last, envelope = i, env
			}
		}
	}

	return last, envelope
}
