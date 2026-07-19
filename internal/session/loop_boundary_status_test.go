package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

// /status is answered off the control plane, but answering it must not end an
// activation that still owes the model a turn: tool results executed moments
// before the command arrived would be stranded with nobody to read them.
func TestRunLoopStatusAtBoundaryEndsOnlyASettledActivation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		messages     []llmwire.Message
		wantCalls    int
		wantMessages int
		wantAnswer   int
	}{
		{name: "fresh session"},
		{
			name:         "fresh session carrying only its AGENTS.md header",
			messages:     []llmwire.Message{usr(agentsMDMessagePrefix + "be careful")},
			wantMessages: 1,
		},
		{
			name:         "settled session",
			messages:     []llmwire.Message{usr("old question"), asst("old answer")},
			wantMessages: 2,
		},
		{
			name:         "tool results still owed an answer",
			messages:     []llmwire.Message{usr("old task"), asst("", call("read-1", "read"))},
			wantCalls:    1,
			wantMessages: 4,
			wantAnswer:   1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := newTestAgent(&stubTool{id: "read", result: "tool result"})
			agent.maxIterations = 5
			agent.ms.setMessages(tc.messages)
			agent.boundary = &loopInputBoundary{
				agent: agent,
				input: &PendingInput{ID: 1, Content: "/status", ReceivedAt: time.Now()},
			}

			llmClient := &loopScriptLLM{responses: []*llmwire.Response{textResponse("answered")}}
			agent.llmClient = llmClient
			notifier := &loopNotifier{}

			_, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))

			require.NoError(t, err)
			assert.Equal(t, tc.wantCalls, llmClient.calls)
			assert.Equal(t, 1, notifier.countWith("Session Status"), "the status report is delivered exactly once")
			assert.Equal(t, tc.wantAnswer, notifier.countWith("✅ answered"))
			assert.Len(t, agent.ms.getMessages(), tc.wantMessages, "the status command writes nothing itself")
		})
	}
}
