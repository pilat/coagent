package session

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/tool/builtin"
)

func TestCompactionHeaderSize(t *testing.T) {
	tests := []struct {
		name     string
		messages []llmwire.Message
		want     int
	}{
		{
			name:     "empty transcript has no header",
			messages: nil,
			want:     0,
		},
		{
			name: "system message reserves system plus task",
			messages: []llmwire.Message{
				{Role: llmwire.RoleSystem, Content: "sys"},
				{Role: llmwire.RoleUser, Content: "task"},
				{Role: llmwire.RoleUser, Content: "later"},
			},
			want: 2,
		},
		{
			name: "agents md preamble reserves two without a system message",
			messages: []llmwire.Message{
				{Role: llmwire.RoleUser, Content: agentsMDMessagePrefix + "rules"},
				{Role: llmwire.RoleUser, Content: "task"},
				{Role: llmwire.RoleUser, Content: "later"},
			},
			want: 2,
		},
		{
			name:     "system alone cannot reserve a task slot",
			messages: []llmwire.Message{{Role: llmwire.RoleSystem, Content: "sys"}},
			want:     1,
		},
		{
			name: "plain user opening reserves only the task",
			messages: []llmwire.Message{
				{Role: llmwire.RoleUser, Content: "task"},
				{Role: llmwire.RoleUser, Content: "later"},
			},
			want: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, compactionHeaderSize(tc.messages))
		})
	}
}

// testReattachWindow yields a 25k combined reattachment budget (10% of window).
const testReattachWindow = 250000

func TestTruncateSkillReattachmentCapsAtTheRuneBudget(t *testing.T) {
	const maxRunes = skillReattachMaxTokens * skillReattachCharsPerToken

	atBudget := strings.Repeat("界", maxRunes)
	assert.Equal(t, atBudget, truncateSkillReattachment(atBudget, skillReattachMaxTokens))

	overBudget := strings.Repeat("界", maxRunes+1)
	truncated := truncateSkillReattachment(overBudget, skillReattachMaxTokens)

	assert.Equal(t, maxRunes, utf8.RuneCountInString(truncated))
	assert.True(t, strings.HasSuffix(truncated, skillReattachTruncated))
	assert.LessOrEqual(t, estimateSkillTokens(truncated), skillReattachMaxTokens)
}

func TestSelectSkillReattachmentsIgnoresSkillsInForeignToolResults(t *testing.T) {
	rendered := builtin.RenderSkill(&loader.Skill{Name: "review", Content: "Review carefully."}, "")
	messages := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
		{Role: llmwire.RoleTool, ToolName: "read", Content: rendered},
		{Role: llmwire.RoleAssistant, Content: rendered},
	}

	assert.Empty(
		t,
		selectSkillReattachments(messages, compactionHeaderSize(messages), len(messages), testReattachWindow),
	)
}

func TestSelectSkillReattachmentsKeepsSmallerCandidatesAfterOneOverflows(t *testing.T) {
	huge := strings.Repeat("界", skillReattachMaxTokens*skillReattachCharsPerToken+100)
	medium := strings.Repeat("界", 15000)

	messages := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
		skillMessage(t, "small", "tiny instructions"),
		skillMessage(t, "overflow", huge),
		skillMessage(t, "medium", medium),
		skillMessage(t, "huge-a", huge),
		skillMessage(t, "huge-b", huge),
		skillMessage(t, "huge-c", huge),
		skillMessage(t, "huge-d", huge),
	}

	reattachments := selectSkillReattachments(
		messages,
		compactionHeaderSize(messages),
		len(messages),
		testReattachWindow,
	)

	assert.Equal(t,
		[]string{"small", "medium", "huge-a", "huge-b", "huge-c", "huge-d"},
		skillNames(t, reattachments),
	)
}

func TestSelectSkillReattachmentsIsIndependentOfMapIterationOrder(t *testing.T) {
	messages := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
	}

	for i := range 6 {
		messages = append(messages, skillMessage(t, "in-"+string(rune('a'+i)), "inside"))
	}

	summarizeEnd := len(messages)

	for i := range 6 {
		messages = append(messages, skillMessage(t, "out-"+string(rune('a'+i)), "outside"))
	}

	want := []string{"in-a", "in-b", "in-c", "in-d", "in-e", "in-f"}

	// The range filter walks a map, so a break instead of a continue would drop
	// in-range skills only on some iteration orders.
	for range 50 {
		got := selectSkillReattachments(messages, compactionHeaderSize(messages), summarizeEnd, testReattachWindow)
		require.Equal(t, want, skillNames(t, got))
	}
}

func skillMessage(t *testing.T, name, content string) llmwire.Message {
	t.Helper()

	return llmwire.Message{
		Role:    llmwire.RoleUser,
		Content: builtin.RenderSkill(&loader.Skill{Name: name, Content: content}, ""),
	}
}

func skillNames(t *testing.T, messages []llmwire.Message) []string {
	t.Helper()

	names := make([]string, 0, len(messages))

	for _, message := range messages {
		name, _, ok := builtin.ExtractRenderedSkill(message.Content)
		require.True(t, ok)
		names = append(names, name)
	}

	return names
}

// The reattachment budget is a share of the window, not a constant: on a 32k
// model 25k of skills would leave compaction nothing to converge to.
func TestSelectSkillReattachmentsBudgetScalesWithTheWindow(t *testing.T) {
	assert.Equal(t, 25000, skillReattachBudget(250000))
	assert.Equal(t, 3200, skillReattachBudget(32000))

	messages := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
		skillMessage(t, "tiny", "short"),
		skillMessage(t, "fat", strings.Repeat("界", 16000)), // 4000 tokens
	}

	// On a 32k window the per-skill cap drops to the 3.2k combined budget, which
	// the newest candidate then fills on its own.
	small := selectSkillReattachments(messages, compactionHeaderSize(messages), len(messages), 32000)
	assert.Equal(t, []string{"fat"}, skillNames(t, small))
	assert.LessOrEqual(t, estimateSkillTokens(small[0].Content), skillReattachBudget(32000))

	large := selectSkillReattachments(messages, compactionHeaderSize(messages), len(messages), testReattachWindow)
	assert.Equal(t, []string{"tiny", "fat"}, skillNames(t, large), "both fit a 25k budget, in order")
}
