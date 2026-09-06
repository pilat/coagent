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

	collector.waitFor(
		t,
		"status answers while the model call is still in flight",
		func(e []controllerapi.SessionNotification) bool {
			return len(statusReports(e, sessionID)) == 1
		},
	)

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
// after the requested compaction runs. The first round settles before the
// second hangs, so the raw range holds two groups when the /compact lands —
// the never-empty tail needs something to keep.
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

		if hasToolResultFor(msgs, "read") || hasSummaryRow(msgs) {
			return &llmwire.Response{Text: "work done"}
		}

		if hasToolResultFor(msgs, "ls") {
			// The second turn hangs until the /compact has landed mid-activation.
			once.Do(func() { close(entered) })
			<-release

			return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
				ID:        "read-call-1",
				Name:      "read",
				Arguments: []byte(`{"file_path":"go.mod"}`),
			}}}
		}

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

// The durable TODO JSON mirrors a real todowrite: mixed priorities, an exact
// priority/time tie resolved by ID, and one legacy item without a timestamp.
// The full /status list must render canonical order, icon-only rows, no
// priority text, and the legend after one blank line.
func TestHarnessScenario_StatusFullTodoListOrderingAndIcons(t *testing.T) {
	h := newSubagentHarnessWith(t, trivialRespond)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		h.shutdown()
	}()

	root, err := h.mgr.Send(h.ctx, h.projectID, "plan the work", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	h.mgr.waitIdle(root)

	base := time.Now().UTC().Add(-time.Hour)
	stamp := func(d time.Duration) string { return base.Add(d).Format(time.RFC3339Nano) }
	todos := `[` +
		`{"id":"low","content":"low task","status":"pending","priority":"low","created_at":"` + stamp(0) + `"},` +
		`{"id":"high-late","content":"high late","status":"in_progress","priority":"high","created_at":"` + stamp(time.Minute) + `"},` +
		`{"id":"tie-b","content":"tie b","status":"pending","priority":"high","created_at":"` + stamp(0) + `"},` +
		`{"id":"high-early","content":"high early","status":"completed","priority":"high","created_at":"` + stamp(0) + `"},` +
		`{"id":"tie-a","content":"tie a","status":"cancelled","priority":"high","created_at":"` + stamp(0) + `"},` +
		`{"id":"legacy","content":"legacy item","status":"pending","priority":"medium"}` +
		`]`
	require.NoError(t, h.sessStore.UpdateSessionTodoItems(h.ctx, root, []byte(todos)))

	require.NoError(t, h.mgr.SendToSession(h.ctx, root, "/status"))
	collector.waitFor(t, "full /status TODO list", func(e []controllerapi.SessionNotification) bool {
		return len(statusReports(e, root)) > 0
	})
	h.mgr.waitIdle(root)

	reports := statusReports(collector.snapshot(), root)
	report := reports[len(reports)-1]
	assert.Contains(t, report, "- TODO: 1 active · 4 remaining · 1 done · 1 cancelled")
	// Canonical order: priority, then created_at, then ID for exact ties.
	assert.Equal(t, []string{
		"  - ✅ high early",
		"  - 🚫 tie a",
		"  - ⏳ tie b",
		"  - 🔄 high late",
		"  - ⏳ legacy item",
		"  - ⏳ low task",
	}, todoRows(report))
	// Priority is ordering-only metadata, never visible text.
	assert.NotContains(t, report, "[high]")
	assert.NotContains(t, report, "[medium]")
	assert.NotContains(t, report, "[low]")
	assert.NotContains(t, report, "[in_progress]")
	// The legend follows the rows after one blank line.
	assert.Contains(t, report,
		"  - ⏳ low task\n\nLegend: ⏳ pending · 🔄 in progress · ✅ completed · 🚫 cancelled")
	h.requireInboxDrained(root)
}

// todoRows extracts the icon-only TODO rows from a rendered status report.
func todoRows(report string) []string {
	var rows []string

	for line := range strings.SplitSeq(report, "\n") {
		for _, icon := range []string{"⏳", "🔄", "✅", "🚫", "❔"} {
			if strings.HasPrefix(line, "  - "+icon+" ") {
				rows = append(rows, line)
			}
		}
	}

	return rows
}
