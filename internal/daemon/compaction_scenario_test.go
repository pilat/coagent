package daemon

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
)

const contextSummaryPrefix = "[CONTEXT SUMMARY"

// blockingCompactRespond drives a parent that spawns one blocking child and
// answers with a compaction brief whenever it is handed a summarization prompt.
func blockingCompactRespond(release <-chan struct{}) func(string, []llmwire.Message) *llmwire.Response {
	return func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if len(msgs) == 1 && strings.Contains(msgs[0].Content, "HISTORY TO SUMMARIZE") {
			return &llmwire.Response{
				Text: "## Goal\nspawn work\n## Progress\n- child ran\n## Context for Continuation\ncarry on",
			}
		}

		if hasUserContaining(msgs, "CHILD_TASK") {
			if release != nil {
				<-release
			}

			return &llmwire.Response{Text: "blocking child done: 7"}
		}

		// The deferred /compact continues the activation, so the parent may be
		// asked again over the compacted transcript — answer, don't re-spawn.
		if hasToolResultFor(msgs, "task") || hasSummaryRow(msgs) {
			return &llmwire.Response{Text: "parent got the child result"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:        taskCallID,
			Name:      "task",
			Arguments: []byte(`{"prompt":"CHILD_TASK do it","description":"c","subagent_type":"general"}`),
		}}}
	}
}

func hasSummaryRow(msgs []llmwire.Message) bool {
	for _, m := range msgs {
		if strings.HasPrefix(m.Content, contextSummaryPrefix) {
			return true
		}
	}

	return false
}

// The blocker this plan closes: /compact arriving while a blocking child is out
// must not compact the task tool_use away from under the child. The request
// waits in the durable inbox, and compaction runs only once the result is in.
func TestScenario_CompactWaitsForABlockingChildThenRuns(t *testing.T) {
	release := make(chan struct{})

	h := newSubagentHarnessWith(t, blockingCompactRespond(release))

	closed := false
	defer func() {
		if !closed {
			close(release)
		}

		h.shutdown()
	}()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "do work then spawn", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)
	require.True(t, link.Blocking)

	h.waitUntil("parent suspended", func() bool { return !h.mgr.HasActiveLoop(parentID) })

	require.NoError(t, h.mgr.SendToSession(h.ctx, parentID, "/compact"))

	// A non-sleep pending call keeps the session unrunnable, so nothing compacts:
	// the transcript still owes the task call its result.
	msgs := h.parentMessages(parentID)
	assert.False(t, hasSummaryRow(msgs), "compaction must not run while the call is out")
	assert.True(t, hasAssistantToolCall(msgs, "task"), "the tool_use the child owns is still there")
	assert.Zero(t, countToolResultsFor(msgs, "task"))

	close(release)
	closed = true

	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	// The result landed in its own tool_use first; only then did the queued
	// /compact run. Nothing is left dangling for ResolvePendingCall to fail on.
	final := h.parentMessages(parentID)
	assert.True(t, hasSummaryRow(final), "the deferred /compact ran once the call was settled")
	require.NoError(t, llm.ValidateToolPairing(final))
	assert.False(t, hasAssistantToolCall(final, "task"), "the settled pair was compacted normally")

	res, err := h.mgr.Result(h.ctx, link.ChildID)
	require.NoError(t, err)
	assert.Equal(t, LinkStateCompleted, res.State, "no zombie link")
}

// The deferred request rides the durable inbox, so it survives the daemon dying
// between the /compact and the child's completion.
func TestScenario_DeferredCompactSurvivesADaemonRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "restart.db")
	release := make(chan struct{})

	first := newSubagentHarnessOnDB(t, dbPath, blockingCompactRespond(release), nil)

	parentID, err := first.mgr.Send(first.ctx, first.projectID, "do work then spawn", "fake-model", nil)
	require.NoError(t, err)

	link := first.waitForChildLink(parentID)
	require.True(t, link.Blocking)

	first.waitUntil("parent suspended", func() bool { return !first.mgr.HasActiveLoop(parentID) })
	require.NoError(t, first.mgr.SendToSession(first.ctx, parentID, "/compact"))

	require.False(t, hasSummaryRow(first.parentMessages(parentID)))

	// Daemon goes down with the child still running and /compact still queued.
	close(release)
	first.shutdown()

	// A second daemon on the same durable state: its sweep resumes the child,
	// whose completion revives the parent, which then finds the queued /compact.
	second := newSubagentHarnessOnDB(t, dbPath, blockingCompactRespond(nil), nil)
	defer second.shutdown()

	require.NoError(t, second.mgr.Start(second.ctx))

	second.waitUntil("deferred compaction ran after the restart", func() bool {
		return hasSummaryRow(second.parentMessages(parentID))
	})

	require.NoError(t, llm.ValidateToolPairing(second.parentMessages(parentID)))
}
