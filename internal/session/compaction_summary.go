package session

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/tool/builtin"
)

const (
	// Bounded programmatically: the model is never asked to reproduce text it
	// might paraphrase instead.
	compactionExcerptCount = 3
	compactionExcerptChars = 600

	compactionExcerptHeader = "\n\n## Verbatim tail (exact text of the last turns before compaction)\n"
)

// syntheticContentPrefixes mark scaffolding the loop injects. Quoting it back
// would cite the machinery instead of the work.
var syntheticContentPrefixes = []string{
	compactionSummaryPrefix,
	compactionPrimerPrefix,
	"[AUTOMATED WARNING",
	"[LOOP WARNING",
	"[BLOCKED",
	agentsMDMessagePrefix,
}

// summaryTurn is everything the compacted transcript keeps. Only the brief feeds
// the next merge — quoting stale excerpts back would retell yesterday's tail.
type summaryTurn struct {
	brief      string
	verbatim   string
	background string
}

func (t summaryTurn) render() string {
	return compactionSummaryPrefix + " - previous work condensed]\n\n" + t.brief + t.verbatim + t.background
}

// buildVerbatimTail quotes the last few substantive turns: the exact diff or
// stack trace a paraphrase reliably loses.
func buildVerbatimTail(messages []llmwire.Message) string {
	picked := make([]llmwire.Message, 0, compactionExcerptCount)

	for _, msg := range slices.Backward(messages) {
		if len(picked) == compactionExcerptCount {
			break
		}

		if quotable(msg) {
			picked = append(picked, msg)
		}
	}

	if len(picked) == 0 {
		return ""
	}

	slices.Reverse(picked)

	var b strings.Builder

	b.WriteString(compactionExcerptHeader)

	for _, msg := range picked {
		fmt.Fprintf(&b, "\n[%s]: %s\n", msg.Role, truncateHeadTail(msg.Content, compactionExcerptChars))
	}

	return b.String()
}

// quotable reports whether a message is the user's or the model's own words —
// not a tool result, and not a synthetic row the loop wrote itself.
func quotable(msg llmwire.Message) bool {
	if msg.Role != llmwire.RoleUser && msg.Role != llmwire.RoleAssistant {
		return false
	}

	if strings.TrimSpace(msg.Content) == "" || msg.Content == registry.PostCompactionAssistantAck {
		return false
	}

	for _, prefix := range syntheticContentPrefixes {
		if strings.HasPrefix(msg.Content, prefix) {
			return false
		}
	}

	return len(builtin.ExtractRenderedSkills(msg.Content)) == 0
}

// activeBackgroundSection renders the children still running, so a later
// "subagent #42 completed" lands where #42 still means something.
func (s *svc) activeBackgroundSection(ctx context.Context) string {
	if s.activeSubagentsProvider == nil {
		return ""
	}

	return buildActiveSubagentsSection(s.activeSubagentsProvider(ctx))
}
