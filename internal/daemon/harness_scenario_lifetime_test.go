package daemon

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

// childHoldRounds is where the scenario pauses the child mid-run: long enough
// for the spawn to settle, short enough to keep the recorded trace readable.
const childHoldRounds = 3

const childTotalRounds = 12

// TestHarnessScenario_ForegroundChildHasNoLifetimeLimit drives one manager-owned
// root through a foreground explore child that crosses the former per-type
// iteration cap and finishes on its own. The task-call JSON still carries the
// obsolete "timeout" key: historical arguments must decode permissively and
// never resurrect a wall-clock deadline.
func TestHarnessScenario_ForegroundChildHasNoLifetimeLimit(t *testing.T) {
	childRelease := make(chan struct{})

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_LONG_RUN") {
			rounds := countToolResultsFor(msgs, "ls")
			switch {
			case rounds >= childTotalRounds:
				return &llmwire.Response{Text: "long child finished"}
			case rounds == childHoldRounds:
				<-childRelease
			}

			return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
				ID:        fmt.Sprintf("ls-%d", rounds+1),
				Name:      "ls",
				Arguments: []byte(`{"path":"."}`),
			}}}
		}

		if hasToolResultFor(msgs, tool.IDTask) {
			return &llmwire.Response{Text: "parent collected the long child"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:   taskCallID,
			Name: tool.IDTask,
			Arguments: []byte(
				`{"prompt":"CHILD_LONG_RUN","description":"scenario","subagent_type":"explore","timeout":1}`,
			),
		}}}
	}

	h := newSubagentHarnessWith(t, respond)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	released := false
	defer func() {
		if !released {
			close(childRelease)
		}

		collector.stop()
		h.shutdown()
	}()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "run the long child", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)

	// While the child is held mid-run, all four durable wait facts hold at once.
	h.waitUntil("child link spawned", func() bool {
		link, linkErr := h.links.GetLinkByTaskCallID(h.ctx, parentID, taskCallID)

		return linkErr == nil && link != nil
	})
	h.waitUntil("parent suspended on the child", func() bool {
		rec, recErr := h.sessStore.GetSession(h.ctx, parentID)

		return recErr == nil && rec.Status == sessionstore.SessionStatusSuspended
	})
	link, err := h.links.GetLinkByTaskCallID(h.ctx, parentID, taskCallID)
	require.NoError(t, err)

	parentRec, err := h.sessStore.GetSession(h.ctx, parentID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusSuspended, parentRec.Status,
		"the parent is durably suspended on the blocking child")
	childRec, err := h.sessStore.GetSession(h.ctx, link.ChildID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusActive, childRec.Status,
		"the child runs with no deadline over its head")
	assert.True(t, link.Blocking)
	assert.Equal(t, subagent.StateSpawned, link.State)
	assert.Zero(t, link.DeliveredAt, "the blocking link is undelivered")
	parentMsgs := h.parentMessages(parentID)
	assert.Zero(t, countToolResultsFor(parentMsgs, tool.IDTask),
		"the parent's task call is still unresolved")
	waitForWaitKind(t, collector, parentID, sessionevent.WaitSubagent)

	close(childRelease)
	released = true

	waitForVisibleMessage(t, collector, parentID, "parent collected the long child")
	drainScenarioClaims(t, "foreground_child_no_lifetime.json", newChainController(t, h))
	waitForIdleAfterMessage(t, collector, parentID, "parent collected the long child")

	link, err = h.links.GetLink(h.ctx, link.ChildID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.OutcomeCompleted, link.Outcome)

	child := h.parentMessages(link.ChildID)
	require.NoError(t, llm.ValidateToolPairing(child))
	assert.Greater(t, countToolResultsFor(child, "ls"), 10,
		"the explore child crossed the former tenth-iteration cap on its own work")

	final := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(final))
	assert.Equal(t, 1, countToolResultsFor(final, tool.IDTask),
		"exactly one parent result resolves the task call")

	assertHarnessTrace(t, "foreground_child_no_lifetime.json", collector.snapshot(), parentID)
}

// TestScenario_RunnerAddsNoChildLifetimeDeadline uses the raw daemon/session
// seam: the child's own LLM client records the context its Chat calls ran
// under. The runner must hand the child only the explicit cancellation context
// — no wall-clock deadline derived from the link, the agent type, or anything
// else. Explicit stop still reaches that same context.
func TestScenario_RunnerAddsNoChildLifetimeDeadline(t *testing.T) {
	childRelease := make(chan struct{})

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_SEAM") {
			<-childRelease

			return &llmwire.Response{Text: "unreached by the happy path"}
		}

		if hasToolResultFor(msgs, tool.IDTask) {
			return &llmwire.Response{Text: "parent sees the stopped child"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:   taskCallID,
			Name: tool.IDTask,
			Arguments: []byte(
				`{"prompt":"CHILD_SEAM","description":"scenario","subagent_type":"general"}`,
			),
		}}}
	}

	h := newSubagentHarnessWith(t, respond)
	defer func() {
		closeOnce(childRelease)
		h.shutdown()
	}()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn for seam check", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)
	childSessionID := fmt.Sprintf("%d:%d", parentID, link.ChildID)

	// Wait until the child's client is actually inside a Chat call, then check
	// the context it was handed: no deadline may ride along.
	h.waitUntil("child Chat is in flight", func() bool {
		client := h.sessionClient(childSessionID)

		return client != nil && client.hasChatContext()
	})
	childClient := h.sessionClient(childSessionID)
	require.NotNil(t, childClient)
	assert.False(t, childClient.chatRanWithDeadline(),
		"the runner must add no child-lifetime deadline")

	// The only interrupt path is explicit stop: it must cancel the very context
	// the child's client holds.
	require.NoError(t, h.mgr.Stop(h.ctx, link.ChildID, 0))
	h.waitUntil("stop cancels the child context", childClient.sawCancellation)
	assert.Equal(t, subagent.StateStopped, func() subagent.State {
		l, err := h.links.GetLink(h.ctx, link.ChildID)
		require.NoError(t, err)
		require.NotNil(t, l)

		return l.State
	}(), "an explicit stop parks the child")
}
