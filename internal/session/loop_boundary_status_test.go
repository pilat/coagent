package session

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

type commandFollowupBoundary struct {
	*loopInputBoundary
	followup PendingInput
}

type rejectingInputBoundary struct {
	*loopInputBoundary
}

func (b *rejectingInputBoundary) Reject(context.Context, PendingInput, string) error {
	b.input = nil

	return nil
}

func (b *commandFollowupBoundary) Handle(context.Context, PendingInput, string) error {
	b.input = &b.followup

	return nil
}

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
			assert.Equal(t, tc.wantAnswer, notifier.countWith("answered"))
			assert.Len(t, agent.ms.getMessages(), tc.wantMessages, "the status command writes nothing itself")
		})
	}
}

func TestDrainBoundaryExplicitInputReleasesStoppedPreservation(t *testing.T) {
	for _, tc := range []struct {
		name         string
		source       sessionstore.InputSource
		wantAccepted bool
		wantPreserve bool
	}{
		{name: "user resumes", source: sessionstore.InputSourceUser, wantAccepted: true},
		{name: "agent resumes", source: sessionstore.InputSourceAgent, wantAccepted: true},
		{name: "process remains parked", source: sessionstore.InputSourceProcess, wantPreserve: true},
		{name: "subagent remains parked", source: sessionstore.InputSourceSubagent, wantPreserve: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := newTestAgent()
			agent.preserveStopped = true
			agent.boundary = &loopInputBoundary{
				agent: agent,
				input: &PendingInput{
					ID: 1, Content: "durable input", Source: tc.source, ReceivedAt: time.Now(),
				},
			}

			accepted, err := (&loopRunner{agent: agent}).drainBoundary(t.Context())

			require.NoError(t, err)
			assert.Equal(t, tc.wantAccepted, accepted)
			assert.Equal(t, tc.wantPreserve, agent.preserveStopped)
		})
	}
}

func TestRunLoopExplicitInputArrivingDuringStoppedReadOnlyCommandResumes(t *testing.T) {
	agent := newTestAgent()
	agent.preserveStopped = true
	boundary := &commandFollowupBoundary{
		loopInputBoundary: &loopInputBoundary{
			agent: agent,
			input: &PendingInput{
				ID: 1, Content: "/help", Source: sessionstore.InputSourceUser, ReceivedAt: time.Now(),
			},
		},
		followup: PendingInput{
			ID: 2, Content: "resume normally", Source: sessionstore.InputSourceUser, ReceivedAt: time.Now(),
		},
	}
	agent.boundary = boundary
	agent.llmClient = &loopScriptLLM{responses: []*llmwire.Response{textResponse("resumed")}}
	notifier := &loopNotifier{}

	result, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))

	require.NoError(t, err)
	assert.Equal(t, "resumed", result.FinalResponse)
	assert.False(t, agent.preserveStopped)
	assert.Equal(t, 1, agent.llmClient.(*loopScriptLLM).calls)
	assert.Equal(t, 1, notifier.countWith("Session commands"))
}

func TestDrainBoundaryRejectedExplicitInputPreservesStoppedStatus(t *testing.T) {
	agent := newTestAgent()
	agent.preserveStopped = true
	agent.boundary = &rejectingInputBoundary{loopInputBoundary: &loopInputBoundary{
		agent: agent,
		input: &PendingInput{
			ID: 1, Content: "/skill", Source: sessionstore.InputSourceUser, ReceivedAt: time.Now(),
		},
	}}

	accepted, err := (&loopRunner{agent: agent}).drainBoundary(t.Context())

	require.NoError(t, err)
	assert.False(t, accepted)
	assert.True(t, agent.preserveStopped)
}

func TestRunLoopStoppedReadOnlyCommandLeavesFollowingAsyncInputPending(t *testing.T) {
	agent := newTestAgent()
	agent.preserveStopped = true
	boundary := &commandFollowupBoundary{
		loopInputBoundary: &loopInputBoundary{
			agent: agent,
			input: &PendingInput{
				ID: 1, Content: "/help", Source: sessionstore.InputSourceUser, ReceivedAt: time.Now(),
			},
		},
		followup: PendingInput{
			ID: 2, Content: "<process_completion>",
			Source: sessionstore.InputSourceProcess, ReceivedAt: time.Now(),
		},
	}
	agent.boundary = boundary
	client := &loopScriptLLM{responses: []*llmwire.Response{textResponse("must not run")}}
	agent.llmClient = client
	notifier := &loopNotifier{}

	result, err := runLoop(t.Context(), agent, loopOptions{Notify: notifier.fn}, iterationGuard(5))

	require.NoError(t, err)
	assert.Empty(t, result.FinalResponse)
	assert.True(t, agent.preserveStopped)
	assert.Zero(t, client.calls)
	require.NotNil(t, boundary.input)
	assert.Equal(t, int64(2), boundary.input.ID)
	assert.Equal(t, 1, notifier.countWith("Session commands"))
}
