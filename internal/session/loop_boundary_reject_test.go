package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
)

// rejectingBoundary models the durable inbox: a rejected row is resolved and
// never peeked again, so a spin here is a production spin.
type rejectingBoundary struct {
	input      *PendingInput
	rejections []string
	accepts    int
}

func (b *rejectingBoundary) Peek(context.Context) (*PendingInput, error) { return b.input, nil }

func (b *rejectingBoundary) Accept(
	context.Context,
	PendingInput,
	string,
	[]PendingToolCall,
) (bool, bool, error) {
	b.accepts++

	return true, false, nil
}

func (b *rejectingBoundary) Reject(_ context.Context, _ PendingInput, reason string) error {
	b.rejections = append(b.rejections, reason)
	b.input = nil

	return nil
}

func (b *rejectingBoundary) Handle(context.Context, PendingInput, string) error {
	b.input = nil

	return nil
}

// A /skill nobody can serve is resolved on the control plane, like /status: the
// row is rejected once, the human is told once, and the model is asked for a turn
// only when the transcript already held work that owes an answer.
func TestRunLoopRejectsUnknownSkillAtBoundary(t *testing.T) {
	for _, tc := range []struct {
		name         string
		messages     []llmwire.Message
		wantCalls    int
		wantMessages int
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
			// The rejection must not claim an activation that just produced tool
			// results: those would be stranded with nobody to read them.
			name:         "tool results still owed an answer",
			messages:     []llmwire.Message{usr("old task"), asst("", call("read-1", "read"))},
			wantCalls:    2,
			wantMessages: 6,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := newDurableTestAgent(t, &stubTool{id: "read", result: "tool result"})
			agent.loader = loader.New()
			agent.ms.setMessages(tc.messages)

			boundary := &rejectingBoundary{
				input: &PendingInput{ID: 1, Content: "/skill nope", ReceivedAt: time.Now()},
			}
			agent.boundary = boundary
			agent.turns = newToolTurns(agent.registry, agent.models, agent.ms, testProgressBoundary(agent.boundary))
			agent.contexts = newCheckpointOwner(
				agent.ms, agent.models, agent.prompt, agent.turns, agent.transcript(),
				agent.dispositions, agent.budgetGate, agent.outputStore, agent.boundary,
				&agent.stamper, nil, nil,
				checkpointOptions{id: agent.id, outputEnabled: agent.outputEnabled, agentsMD: agent.agentsMD},
			)

			llmClient := &loopScriptLLM{
				responses: []*llmwire.Response{textResponse("answered"), textResponse("confirmed")},
			}
			agent.models = newTestModelRuntime(llmClient, agent.store, agent.id)
			agent.turns = newToolTurns(agent.registry, agent.models, agent.ms, testProgressBoundary(agent.boundary))
			agent.contexts = newCheckpointOwner(
				agent.ms, agent.models, agent.prompt, agent.turns, agent.transcript(),
				agent.dispositions, agent.budgetGate, agent.outputStore, agent.boundary,
				&agent.stamper, nil, nil,
				checkpointOptions{id: agent.id, outputEnabled: agent.outputEnabled, agentsMD: agent.agentsMD},
			)
			notifier := &loopNotifier{}

			_, err := runTestLoop(t.Context(), t, agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))

			require.NoError(t, err)
			assert.Equal(t, tc.wantCalls, llmClient.calls)
			assert.Zero(t, boundary.accepts)
			require.Len(t, boundary.rejections, 1)
			assert.Contains(t, boundary.rejections[0], "skill unavailable: nope")
			assert.Equal(t, 1, notifier.countWith("⚠️ skill unavailable: nope"))
			assert.Len(t, agent.ms.getMessages(), tc.wantMessages, "the rejected command writes nothing itself")
		})
	}
}
