package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestStopHookContinuationLoopPublishesReplacementAnswer(t *testing.T) {
	_, store, agent := newBoundaryFixture(t, nil)
	_, err := store.Enqueue(t.Context(), sessionstore.Input{
		SessionID: agent.id, Source: sessionstore.InputSourceUser, Content: "complete the request",
	})
	require.NoError(t, err)
	responses := []*llmwire.Response{
		textResponse("answer A"), textResponse("confirmed"),
		toolCallResponse("verify-1", "verify"), textResponse("answer B"),
	}
	sawReason := false
	agent.llmClient = &loopScriptLLM{onCall: func(index int, messages []llmwire.Message) (*llmwire.Response, error) {
		require.LessOrEqual(t, index, len(responses))
		for _, message := range messages {
			if message.Role == llmwire.RoleUser && strings.Contains(message.Content, "verify it first") {
				sawReason = true
			}
		}
		return responses[index-1], nil
	}}
	counter := &countingTool{id: "verify"}
	agent.registry.Register(counter)
	hook := &stopHookFixture{reasons: []string{"verify it first", ""}}
	agent.stopHook = hook
	var notes []string
	agent.events = noteEvents{notes: &notes}
	result, err := agent.Run(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "answer B", result.Final)
	assert.Equal(t, int64(1), counter.runs.Load())
	assert.Equal(t, []string{"answer A", "answer B"}, hook.messages)
	assert.Equal(t, []bool{false, true}, hook.active)
	assert.True(t, sawReason)
	assert.Equal(t, []string{"answer B"}, notes)
	assertSingleStopHookOutput(t, store, agent.id)
}

func assertSingleStopHookOutput(t *testing.T, store *sessionstore.Store, id int64) {
	t.Helper()
	outputs, err := store.ChatOutputs(t.Context(), id, 0, 100)
	require.NoError(t, err)
	var published []string
	for _, output := range outputs {
		if output.Type == sessionstore.OutputMessage {
			published = append(published, output.Content)
		}
	}
	assert.Equal(t, []string{"answer B"}, published)
}
