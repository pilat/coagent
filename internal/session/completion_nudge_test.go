package session

import (
	"testing"
	"time"

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

func TestFinalPartsYield(t *testing.T) {
	for _, yield := range []bool{false, true} {
		agent := newTestAgent()
		agent.omitFinalFooter = true
		step := agent.newCommit()
		run := &runState{}
		state := &sessionstore.CompletionCheckState{NudgedThisGeneration: true}
		agent.prepareStopResponse(run, &step, &llmwire.Response{Text: "answer"}, state, yield)
		require.Len(t, step.Unfired.Outputs, 1)
		assert.Equal(t, yield, step.Unfired.Outputs[0].Yield)
		assert.Nil(t, step.Unfired.Outputs[0].FinalFooter)
	}
}

func TestPrepareAcceptedAttempt_SubagentBackgroundExit(t *testing.T) {
	live := []sessionstore.LiveProcess{
		{ID: "bgp_7", ToolCallID: "search", CreatedAt: time.Now().UTC().Add(-time.Minute)},
	}
	tests := []struct {
		name      string
		parentID  int64
		live      []sessionstore.LiveProcess
		otherWake bool
		nudged    bool
		wantNudge bool
		wantYield bool
	}{
		{name: "subagent with live process is nudged", parentID: 1, live: live, wantNudge: true},
		{name: "subagent stop after the nudge yields", parentID: 1, live: live, nudged: true, wantYield: true},
		{name: "root keeps the background yield", live: live, wantYield: true},
		{name: "subagent waiting on another source yields", parentID: 1, otherWake: true, wantYield: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			agent := newTestAgent()
			agent.id, agent.parentID = 2, tt.parentID
			store, ok := agent.store.(*mockSessionStore)
			require.True(t, ok)
			store.live, store.wake = tt.live, tt.otherWake
			store.completion = sessionstore.CompletionCheckState{NudgedThisGeneration: tt.nudged}
			setTestMessages(agent, []llmwire.Message{{
				Role: llmwire.RoleAssistant,
				ToolCalls: []llmwire.ToolCall{
					{
						ID:        "search",
						Name:      "bash",
						Arguments: []byte(`{"command":"curl -s https://example.test","background":true}`),
					},
				},
			}})

			c := agent.newCommit()
			run := &runState{}
			require.NoError(t, agent.prepareAcceptedAttempt(t.Context(), run, &c, &llmwire.Response{Text: "report"}))

			assert.Equal(t, tt.wantYield, run.terminal)
			assert.Equal(t, tt.wantNudge, c.Unfired.State.MarkCompletionNudge)
			if tt.wantNudge {
				require.Len(t, c.Unfired.Messages, 1)
				assert.Contains(
					t,
					c.Unfired.Messages[0].Content,
					"- bgp_7 (running 1m0s): curl -s https://example.test",
				)
				assert.Empty(t, c.Unfired.Outputs, "a nudged stop publishes nothing")
				assert.Nil(t, c.Unfired.State.Candidate, "the background nudge opens no completion candidate")
			}
			if tt.wantYield {
				assert.Empty(t, c.Unfired.Messages)
				require.Len(t, c.Unfired.Outputs, 1)
				assert.True(t, c.Unfired.Outputs[0].FinalFooter.BackgroundYield)
			}
		})
	}
}
