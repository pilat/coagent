package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

type stopHookFixture struct {
	reasons  []string
	messages []string
	active   []bool
	cancel   context.CancelFunc
}

func TestStopHookBlockOnFinalBranchHidesAnswerAndNudges(t *testing.T) {
	agent := newTestAgent()
	hook := &stopHookFixture{reasons: []string{"verify the answer"}}
	agent.stopHook = hook
	candidate := int64(17)
	state := &sessionstore.CompletionCheckState{CandidateID: &candidate, CandidateText: "refused answer"}
	c := agent.newCommit()
	run := &runState{}
	require.NoError(t, agent.prepareStopResponse(t.Context(), run, &c, textResponse("confirmed"), state, false))
	assertStopHookContinuation(t, c, run, "verify the answer")
	require.NotNil(t, c.State.Candidate)
	assert.Equal(t, int64(17), c.State.Candidate.Expected)
	assert.Equal(t, -1, c.State.Candidate.NextRef)
	assert.Nil(t, c.State.ConfirmedAnswerID)
	assert.Equal(t, []string{"refused answer"}, hook.messages)
}

func TestStopHookBlockOnWakeYield(t *testing.T) {
	agent := newTestAgent()
	agent.stopHook = &stopHookFixture{reasons: []string{"wait for validation"}}
	c := agent.newCommit()
	run := &runState{}
	require.NoError(t, agent.prepareStopResponse(t.Context(), run, &c, textResponse("waiting"), nil, true))
	assertStopHookContinuation(t, c, run, "wait for validation")
}

func TestStopHookNotRunAtCompletionNudge(t *testing.T) {
	agent := newTestAgent()
	hook := &stopHookFixture{reasons: []string{"not yet"}}
	agent.stopHook = hook
	c := agent.newCommit()
	run := &runState{}
	require.NoError(t, agent.prepareStopResponse(t.Context(), run, &c, textResponse("candidate"), nil, false))
	assert.Empty(t, hook.messages)
	assert.False(t, run.terminal)
	assert.True(t, c.Unfired.State.MarkCompletionNudge)
	require.NotNil(t, c.Unfired.State.Candidate)
	assert.Equal(t, 0, c.Unfired.State.Candidate.NextRef)
	assert.Empty(t, c.Unfired.Outputs)
}

func TestStopHookNotRunForSubagent(t *testing.T) {
	agent := newTestAgent()
	agent.parentID = 42
	hook := &stopHookFixture{reasons: []string{"not for subagents"}}
	agent.stopHook = hook
	c := agent.newCommit()
	run := &runState{}
	state := &sessionstore.CompletionCheckState{NudgedThisGeneration: true}
	require.NoError(t, agent.prepareStopResponse(t.Context(), run, &c, textResponse("answer"), state, false))
	assert.Empty(t, hook.messages)
	assert.True(t, run.terminal)
	assert.Equal(t, "answer", run.result.Final)
}

func TestStopHookAllowPublishesAsToday(t *testing.T) {
	for _, wake := range []bool{false, true} {
		agent := newTestAgent()
		hook := &stopHookFixture{}
		agent.stopHook = hook
		c := agent.newCommit()
		run := &runState{}
		state := &sessionstore.CompletionCheckState{NudgedThisGeneration: true}
		require.NoError(t, agent.prepareStopResponse(t.Context(), run, &c, textResponse("answer"), state, wake))
		assert.True(t, run.terminal)
		assert.Equal(t, "answer", run.result.Final)
		require.Len(t, c.Unfired.Outputs, 1)
		assert.Equal(t, "answer", c.Unfired.Outputs[0].Content)
		assert.Equal(t, []string{"answer"}, hook.messages)
		assert.Equal(t, []bool{false}, hook.active)
	}
}

func TestStopHookActiveAfterBlock(t *testing.T) {
	agent := newTestAgent()
	hook := &stopHookFixture{reasons: []string{"first check", "second check"}}
	agent.stopHook = hook
	run := &runState{}
	state := &sessionstore.CompletionCheckState{NudgedThisGeneration: true}
	for range 2 {
		c := agent.newCommit()
		require.NoError(t, agent.prepareStopResponse(t.Context(), run, &c, textResponse("answer"), state, false))
	}
	assert.Equal(t, []bool{false, true}, hook.active)
	assert.Equal(t, 2, run.stopHookContinuations)
}

func TestStopHookCapPublishesAfterEight(t *testing.T) {
	agent := newTestAgent()
	hook := &stopHookFixture{reasons: make([]string, 9)}
	for i := range hook.reasons {
		hook.reasons[i] = "continue checking"
	}
	agent.stopHook = hook
	run := &runState{}
	state := &sessionstore.CompletionCheckState{NudgedThisGeneration: true}
	for range 8 {
		c := agent.newCommit()
		require.NoError(t, agent.prepareStopResponse(t.Context(), run, &c, textResponse("answer"), state, false))
		assertStopHookContinuation(t, c, run, "continue checking")
	}
	c := agent.newCommit()
	require.NoError(t, agent.prepareStopResponse(t.Context(), run, &c, textResponse("answer"), state, false))
	assert.Len(t, hook.messages, 8)
	assert.Equal(t, 8, run.stopHookContinuations)
	assert.True(t, run.terminal)
	assert.Equal(t, "answer", run.result.Final)
	require.Len(t, c.Unfired.Outputs, 1)
}

func TestStopHookAdditionalContextContinues(t *testing.T) {
	agent := newTestAgent()
	agent.stopHook = &stopHookFixture{reasons: []string{"additional review context"}}
	c := agent.newCommit()
	run := &runState{}
	state := &sessionstore.CompletionCheckState{NudgedThisGeneration: true}
	require.NoError(t, agent.prepareStopResponse(t.Context(), run, &c, textResponse("answer"), state, false))
	assertStopHookContinuation(t, c, run, "additional review context")
}

func TestStopHookCancelledCommitsNothing(t *testing.T) {
	agent := newTestAgent()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	agent.stopHook = &stopHookFixture{cancel: cancel}
	agent.llmClient = &loopScriptLLM{responses: []*llmwire.Response{textResponse("answer")}}
	store := agent.store.(*mockSessionStore)
	store.completion.NudgedThisGeneration = true
	setTestMessages(agent, []llmwire.Message{usr("request")})
	before := store.insertCalls
	run := &runState{}
	err := agent.modelStep(ctx, run)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, before, store.insertCalls)
	assert.Len(t, store.messages, 1)
	assert.Empty(t, run.result.ErrorNotice)
	assert.False(t, run.terminal)
}

func (h *stopHookFixture) Stop(ctx context.Context, lastMessage string, active bool) (string, error) {
	h.messages = append(h.messages, lastMessage)
	h.active = append(h.active, active)
	if h.cancel != nil {
		h.cancel()
		return "", ctx.Err()
	}
	index := len(h.messages) - 1
	if index < len(h.reasons) {
		return h.reasons[index], nil
	}
	return "", nil
}

func assertStopHookContinuation(t *testing.T, c sessionstore.Commit, run *runState, reason string) {
	t.Helper()
	assert.Empty(t, c.Unfired.Outputs)
	assert.Empty(t, c.Outputs)
	assert.False(t, run.terminal)
	assert.Empty(t, run.result.Final)
	assert.True(t, c.Unfired.State.MarkCompletionNudge)
	require.Len(t, c.Unfired.Messages, 1)
	assert.Equal(t, llmwire.RoleUser, c.Unfired.Messages[0].Role)
	assert.Contains(t, c.Unfired.Messages[0].Content, reason)
	require.NotNil(t, c.State.Candidate)
	assert.Equal(t, -1, c.State.Candidate.NextRef)
}
