package session

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/transcript"
)

// reasoningBlob stands in for a provider's sealed reasoning payload. The session
// never opens it, so an opaque blob is exactly as good as a real envelope here.
var reasoningBlob = json.RawMessage(`{"opaque":"payload"}`)

// The second turn of a tool-calling conversation is the one that 400s when the
// first turn's reasoning payload is dropped, so assert it reaches the client.
func TestLoopReplaysReasoningPayloadOnTheNextTurn(t *testing.T) {
	var secondTurn []llmwire.Message

	llmClient := &loopScriptLLM{onCall: func(call int, msgs []llmwire.Message) (*llmwire.Response, error) {
		if call == 1 {
			return &llmwire.Response{
				FinishType:   "tool_calls",
				ToolCalls:    []llmwire.ToolCall{{ID: "tc_1", Name: "read", Arguments: []byte(`{}`)}},
				ReasoningRaw: reasoningBlob,
			}, nil
		}

		secondTurn = msgs

		return &llmwire.Response{Text: "done"}, nil
	}}

	agent := newTestAgent(&stubTool{id: "read", result: "content"})
	agent.llmClient = llmClient
	agent.maxIterations = 5

	_, err := runLoop(t.Context(), agent, loopOptions{}, iterationGuard(5))
	require.NoError(t, err)

	var assistant *llmwire.Message

	for i := range secondTurn {
		if secondTurn[i].Role == llmwire.RoleAssistant {
			assistant = &secondTurn[i]
			break
		}
	}

	require.NotNil(t, assistant, "the first turn's assistant message must be in the second request")
	assert.JSONEq(t, string(reasoningBlob), string(assistant.ReasoningRaw))
}

func TestStoredMessageCarriesReasoningRaw(t *testing.T) {
	stored, err := storedMessage(&llmwire.Message{
		Role:         llmwire.RoleAssistant,
		Content:      "hi",
		ReasoningRaw: reasoningBlob,
	})
	require.NoError(t, err)
	assert.JSONEq(t, string(reasoningBlob), string(stored.ReasoningRaw))
}

func TestReloadMessagesRestoresReasoningRaw(t *testing.T) {
	ms := newMessageStore(&reasoningLoadStore{
		messages: []*transcript.Message{
			{ID: 1, Role: llmwire.RoleUser, Content: "go"},
			{ID: 2, Role: llmwire.RoleAssistant, Content: "sure", ReasoningRaw: reasoningBlob},
		},
	}, 1)

	require.NoError(t, ms.reloadMessages(context.Background()))

	msgs := ms.getMessages()
	require.Len(t, msgs, 2)
	assert.Nil(t, msgs[0].ReasoningRaw)
	assert.JSONEq(t, string(reasoningBlob), string(msgs[1].ReasoningRaw))
}

// reasoningLoadStore serves a fixed transcript so the load-time projection can be
// asserted without a real database.
type reasoningLoadStore struct {
	mockSessionStore

	messages []*transcript.Message
}

func (s *reasoningLoadStore) LoadActiveMessages(
	context.Context, int64,
) ([]*transcript.Message, error) {
	return s.messages, nil
}
