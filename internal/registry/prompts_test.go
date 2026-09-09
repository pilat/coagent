package registry

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBuildAgentPrompt_SubagentResultsAvoidDuplicateResearch(t *testing.T) {
	t.Parallel()

	assert.Contains(t, BuildAgentPrompt, "Do not repeat its searches as routine verification")
	assert.Contains(t, BuildAgentPrompt, "Use explore's supported findings directly")
	assert.Contains(t, BuildAgentPrompt, "resolve a small gap locally")
	assert.NotContains(t, BuildAgentPrompt, "continue the same subagent with a focused question")
	assert.Contains(t, BuildAgentPrompt, "inspect its diff and run the relevant verification")
	assert.NotContains(t, BuildAgentPrompt, "Always sanity-check subagent results")
}

func TestBuiltInPrompts_AvoidRigidToolCallHeuristicsAndCeremonialPrefixes(t *testing.T) {
	t.Parallel()

	for _, prompt := range []string{BuildAgentPrompt, GeneralAgentPrompt, ExploreAgentPrompt} {
		assert.NotContains(t, prompt, "3 or fewer tool calls")
		assert.NotContains(t, prompt, "4+ sequential actions")
		assert.NotContains(t, prompt, "TASK_COMPLETE:")
	}

	assert.NotContains(t, BuildAgentPrompt, "Sensible defaults for vague parameters")
	assert.NotContains(t, BuildAgentPrompt, "tasks with 3+ steps")
	assert.NotContains(t, ExploreAgentPrompt, "10-15 tool calls")
	assert.Contains(t, ExploreAgentPrompt, "Stop when the evidence answers the question")
}

func TestBuiltInPrompts_ParallelizeOnlyIndependentWork(t *testing.T) {
	t.Parallel()

	assert.Contains(t, BuildAgentPrompt, "Never parallelize dependent steps")
	assert.Contains(t, GeneralAgentPrompt, "bounded, independent subtask")
	assert.Contains(t, ExploreAgentPrompt, "Keep dependent steps sequential")
}

func TestBuildAgentPrompt_DescribesActualSubagentContinuation(t *testing.T) {
	t.Parallel()

	assert.Contains(t, BuildAgentPrompt, "does not receive your conversation history")
	assert.Contains(t, BuildAgentPrompt, "general or custom subagent's assignment")
	assert.Contains(t, BuildAgentPrompt, "numeric subagent_id shown in the task result")
	assert.Contains(t, BuildAgentPrompt, "do not routinely resume it or ask it to confirm its answer")
	assert.NotContains(t, BuildAgentPrompt, "Pass it back to the task tool")
}

func TestBuildAgentPrompt_TeachesCooperativeBackgroundWait(t *testing.T) {
	t.Parallel()

	assert.Contains(t, BuildAgentPrompt, "background process or subagent")
	assert.Contains(t, BuildAgentPrompt, "standalone <WAITING/> line and no tool calls")
	assert.Contains(t, BuildAgentPrompt, "receive its result automatically in a new turn")
	assert.Contains(t, BuildAgentPrompt, "I_WOULD_USE_<WAITING/>")
	assert.Contains(t, GeneralAgentPrompt, "standalone <WAITING/> line and no tool calls")
	assert.Contains(t, CompactionSummaryPrompt, "advertised processes and pending subagents")
}

func TestCompactionPrompt_ForbidsInventedState(t *testing.T) {
	t.Parallel()

	assert.Contains(t, CompactionSummaryPrompt, "Do not invent completed work")
	assert.Contains(t, CompactionSummaryPrompt, "preserve that uncertainty")
}
