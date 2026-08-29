package daemon

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionevent"
)

// The status header a controller receives, duplicated from internal/session on
// purpose: it is the contract with the human, not an implementation detail.
const statusReportHeader = "## Session progress"

func statusReports(events []controllerapi.SessionNotification, sessionID int64) []string {
	var out []string

	for _, event := range events {
		if event.SessionID != sessionID || event.Notification.Type != sessionevent.NotifyMessage {
			continue
		}

		if strings.HasPrefix(event.Notification.Message, statusReportHeader) {
			out = append(out, event.Notification.Message)
		}
	}

	return out
}

// /status answers off the control plane, so it costs no model turn — but it must
// not claim an activation that is mid-flight. The tool result executed a moment
// before the command arrived still owes the human an answer.
func TestHarnessScenario_StatusMidActivationDoesNotStrandJustExecutedToolResults(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})

	var once sync.Once

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(msgs, "ls") {
			return &llmwire.Response{Text: "work done"}
		}

		// Hold the first turn open so /status is durable before the loop reaches
		// the boundary that follows the tool result.
		once.Do(func() { close(entered) })
		<-release

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:        "ls-call-1",
			Name:      "ls",
			Arguments: []byte(`{"path":"."}`),
		}}}
	}

	h := newSubagentHarnessWith(t, respond)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())

	released := false

	defer func() {
		if !released {
			close(release)
		}

		collector.stop()
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "list the workdir", "fake-model", nil)
	require.NoError(t, err)

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the model was never asked for the first turn")
	}

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "/status"))

	close(release)

	released = true

	collector.waitFor(t, "the interrupted work is still answered", func(e []controllerapi.SessionNotification) bool {
		return countPublishedMessage(e, sessionID, "work done") == 1
	})
	h.mgr.waitIdle(sessionID)

	events := collector.snapshot()
	assert.Len(t, statusReports(events, sessionID), 1, "exactly one status report")
	assert.Equal(t, 1, countPublishedMessage(events, sessionID, "work done"), "the answer reaches the human once")

	msgs := h.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countToolResultsFor(msgs, "ls"))
	assert.Equal(t, "work done", lastAssistantTextDTO(msgs))
	assert.False(t, hasUserContaining(msgs, "/status"), "a control command never enters the transcript")
	h.requireInboxDrained(sessionID)
}

// /compact follows the same rule as /status: it may end the activation only
// when nothing owes the model a turn, so interrupted work is still answered
// after the requested compaction runs.
func TestHarnessScenario_CompactMidActivationStillAnswersTheInterruptedWork(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})

	var once sync.Once

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if len(msgs) == 1 && strings.Contains(msgs[0].Content, "HISTORY TO SUMMARIZE") {
			return &llmwire.Response{
				Text: "## Goal\nlist the workdir\n## Progress\n- listed\n## Context for Continuation\nreport back",
			}
		}

		if hasToolResultFor(msgs, "ls") || hasSummaryRow(msgs) {
			return &llmwire.Response{Text: "work done"}
		}

		once.Do(func() { close(entered) })
		<-release

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:        "ls-call-1",
			Name:      "ls",
			Arguments: []byte(`{"path":"."}`),
		}}}
	}

	h := newSubagentHarnessWith(t, respond)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())

	released := false

	defer func() {
		if !released {
			close(release)
		}

		collector.stop()
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "list the workdir", "fake-model", nil)
	require.NoError(t, err)

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the model was never asked for the first turn")
	}

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "/compact"))

	close(release)

	released = true

	collector.waitFor(t, "the requested compaction runs", func(e []controllerapi.SessionNotification) bool {
		return countPublishedMessage(e, sessionID, noticeCompacted) == 1
	})
	h.mgr.waitIdle(sessionID)

	collector.waitFor(t, "the interrupted work is still answered", func(e []controllerapi.SessionNotification) bool {
		return countPublishedMessage(e, sessionID, "work done") == 1
	})

	msgs := h.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.True(t, hasSummaryRow(msgs), "the requested compaction ran")
	h.requireInboxDrained(sessionID)
}

// The opposite edge: with nothing in the session yet, /status must end the
// activation itself rather than hand the provider a conversation asking nothing.
func TestHarnessScenario_StatusOnAFreshSessionCostsNoModelTurn(t *testing.T) {
	rec := &skillRecorder{}
	h := newSubagentHarnessWith(t, rec.wrap(plainRespond))
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())

	defer func() {
		collector.stop()
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "/status", "fake-model", nil)
	require.NoError(t, err)

	collector.waitFor(t, "status report reaches the controller", func(e []controllerapi.SessionNotification) bool {
		return len(statusReports(e, sessionID)) == 1
	})
	h.mgr.waitIdle(sessionID)

	assert.Len(t, statusReports(collector.snapshot(), sessionID), 1, "exactly one status report")
	assert.Empty(t, rec.snapshot(), "a conversation that asks nothing is never sent to the provider")
	assert.Empty(t, h.parentMessages(sessionID), "the status command writes nothing")
	h.requireInboxDrained(sessionID)
}

// Unlike /compact, /status reads state a blocking child cannot invalidate: it is
// answered on the spot and leaves the pending join untouched.
func TestHarnessScenario_StatusIsAnsweredWhileABlockingChildIsOut(t *testing.T) {
	release := make(chan struct{})
	h := newSubagentHarnessWith(t, blockingCompactRespond(release))
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())

	released := false

	defer func() {
		if !released {
			close(release)
		}

		collector.stop()
		h.shutdown()
	}()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "do work then spawn", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)
	require.True(t, link.Blocking)
	h.waitUntil("parent suspended", func() bool { return !h.mgr.HasActiveLoop(parentID) })

	require.NoError(t, h.mgr.SendToSession(h.ctx, parentID, "/status"))
	collector.waitFor(t, "status answered while the child is out", func(e []controllerapi.SessionNotification) bool {
		return len(statusReports(e, parentID)) == 1
	})
	h.waitUntil("status wake finished", func() bool { return !h.mgr.HasActiveLoop(parentID) })

	waiting := h.parentMessages(parentID)
	assert.Equal(t, 1, countAssistantToolCallsFor(waiting, "task"))
	assert.Equal(t, 0, countToolResultsFor(waiting, "task"), "the join must still be owed to the child")

	close(release)

	released = true

	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	collector.waitFor(t, "the parent answers once the child returns", func(e []controllerapi.SessionNotification) bool {
		return countPublishedMessage(e, parentID, "parent got the child result") == 1
	})

	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countToolResultsFor(msgs, "task"))
	assert.Len(t, statusReports(collector.snapshot(), parentID), 1, "still exactly one status report")
}
