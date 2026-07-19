package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

// The daemon rebuilds the session on every wake, so the deferral notice's dedup
// state rides in on CreateOptions and back out on RunResult. A rebuilt session
// carrying it must stay quiet.
func TestSlashCompact_RebuiltSessionDoesNotReAnnounceTheDeferral(t *testing.T) {
	s := newCompactionTestSvc(&compactionMockLLM{contextWindow: 200000})
	s.stagedCalls = map[string]string{"t1": tool.IDTask}
	s.ms.setMessages(pendingCallTranscript("t1", tool.IDTask))
	s.compactionDeferAnnounced = true // what the previous wake handed back

	var notes []string
	r, b := compactCommandRunner(s, compactCommand, &notes)

	outcome, err := r.handleBoundaryCommand(t.Context(), *b.input)
	require.NoError(t, err)

	assert.Equal(t, commandDeferred, outcome)
	assert.Empty(t, notes, "the human was already told this episode")
	assert.True(t, s.compactionDeferAnnounced, "the verdict is handed on to the next wake")
}

// The episode is scoped to the call that caused it: once nothing is out with the
// world, a flag carried in from an earlier one must not silence the next notice.
func TestSlashCompact_DeferralEpisodeEndsWithThePendingCall(t *testing.T) {
	s := newCompactionTestSvc(&compactionMockLLM{contextWindow: 200000})
	s.ms.setMessages([]llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		compactionUserMessage("task"),
		{Role: llmwire.RoleAssistant, Content: "settled"},
	})
	s.compactionDeferAnnounced = true

	_, err := runLoop(t.Context(), s, loopOptions{}, nil)
	require.NoError(t, err)

	assert.False(t, s.compactionDeferAnnounced)
}
