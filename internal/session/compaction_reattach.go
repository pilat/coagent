package session

import (
	"cmp"
	"slices"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/tool/builtin"
)

const (
	skillReattachMaxTokens = 5000
	// Proportional: a fixed 25k was 92% of the trigger on a 32k window.
	skillReattachWindowShare   = 0.1
	skillReattachCharsPerToken = 4
	skillReattachTruncated     = "\n[Skill content truncated during compaction]\n</skill>"
)

type skillReattachment struct {
	content      string
	messageIndex int
	order        int
}

// skillReattachBudget is the combined token allowance for everything reattached
// after a compaction.
func skillReattachBudget(window int) int {
	return int(skillReattachWindowShare * float64(window))
}

func selectSkillReattachments(
	messages []llmwire.Message,
	summarizeStart, summarizeEnd int,
	window int,
) []llmwire.Message {
	combinedBudget := skillReattachBudget(window)
	perSkill := min(skillReattachMaxTokens, combinedBudget)

	latest := make(map[string]skillReattachment)
	order := 0

	for i, message := range messages {
		if message.Role != llmwire.RoleUser &&
			(message.Role != llmwire.RoleTool ||
				(message.ToolName != tool.IDSkill && message.ToolName != tool.IDBatch)) {
			continue
		}

		for _, invocation := range builtin.ExtractRenderedSkills(message.Content) {
			latest[invocation.Name] = skillReattachment{
				content:      invocation.Envelope,
				messageIndex: i,
				order:        order,
			}
			order++
		}
	}

	candidates := make([]skillReattachment, 0, len(latest))
	for _, invocation := range latest {
		if invocation.messageIndex < summarizeStart || invocation.messageIndex >= summarizeEnd {
			continue
		}

		invocation.content = truncateSkillReattachment(invocation.content, perSkill)
		candidates = append(candidates, invocation)
	}

	slices.SortFunc(candidates, func(a, b skillReattachment) int {
		return cmp.Compare(b.order, a.order)
	})

	selected := make([]skillReattachment, 0, len(candidates))
	totalTokens := 0

	for _, candidate := range candidates {
		tokens := estimateSkillTokens(candidate.content)
		// A candidate that does not fit what is left is skipped whole, never
		// trimmed to the remainder: half a skill envelope teaches nothing.
		if totalTokens+tokens > combinedBudget {
			continue
		}

		selected = append(selected, candidate)
		totalTokens += tokens
	}

	slices.SortFunc(selected, func(a, b skillReattachment) int {
		return cmp.Compare(a.order, b.order)
	})

	result := make([]llmwire.Message, len(selected))
	for i, invocation := range selected {
		result[i] = llmwire.Message{Role: llmwire.RoleUser, Content: invocation.content}
	}

	return result
}

func truncateSkillReattachment(content string, maxTokens int) string {
	maxRunes := maxTokens * skillReattachCharsPerToken
	runes := []rune(content)

	if len(runes) <= maxRunes {
		return content
	}

	suffix := []rune(skillReattachTruncated)

	return string(runes[:maxRunes-len(suffix)]) + skillReattachTruncated
}

func estimateSkillTokens(content string) int {
	return len([]rune(content)) / skillReattachCharsPerToken
}
