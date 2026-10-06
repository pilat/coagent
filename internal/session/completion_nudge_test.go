package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestPrepareStopResponseAfterNudge(t *testing.T) {
	agent := newTestAgent()
	state := &sessionstore.CompletionCheckState{NudgedThisGeneration: true, ManagerReplyPending: true}

	final := agent.newCommit()
	finalRun := &runState{}
	agent.prepareStopResponse(finalRun, &final, &llmwire.Response{Text: "complete answer"}, state, false)
	assert.True(t, finalRun.terminal)
	assert.Equal(t, "complete answer", finalRun.result.Final)
	assert.Empty(t, final.Unfired.Messages)
	require.Len(t, final.Unfired.Outputs, 1)
	assert.Equal(t, "complete answer", final.Unfired.Outputs[0].Content)
	assert.Nil(t, final.State.ConfirmedAnswerID)

	empty := agent.newCommit()
	emptyRun := &runState{}
	agent.prepareStopResponse(emptyRun, &empty, &llmwire.Response{}, state, false)
	assert.False(t, emptyRun.terminal)
	require.Len(t, empty.Unfired.Messages, 1)
	assert.Contains(t, empty.Unfired.Messages[0].Content, "empty response")
	assert.False(t, empty.Unfired.State.MarkCompletionNudge)

	wake := agent.newCommit()
	wakeRun := &runState{}
	agent.prepareStopResponse(wakeRun, &wake, &llmwire.Response{Text: "waiting"}, state, true)
	assert.True(t, wakeRun.terminal)
	assert.Empty(t, wake.Unfired.Messages)
	require.Len(t, wake.Unfired.Outputs, 1)
	assert.True(t, wake.Unfired.Outputs[0].FinalFooter.BackgroundYield)
}
