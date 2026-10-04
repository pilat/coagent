package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

func TestIntegration_BlockingTaskSuspendsAndResumes(t *testing.T) {
	release := make(chan struct{})

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_TASK") {
			<-release // hold the child so we can observe the suspended parent
			return &llmwire.Response{Text: "blocking child done: 7"}
		}

		if hasToolResultFor(msgs, "task") {
			return &llmwire.Response{Text: "parent got the child result"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:        taskCallID,
			Name:      "task",
			Arguments: []byte(`{"prompt":"CHILD_TASK do it","description":"c","subagent_type":"general"}`),
		}}}
	}

	h := newHarness(t, harnessOptions{respond: respond})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}

		h.shutdown()
	}()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "do work then spawn", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)
	assert.True(t, link.Blocking, "blocking task must create a blocking link")

	// The suspended parent must hold NO run-slot: its loop goroutine exits while
	// it waits for the child (kills the priority-inversion deadlock).
	h.waitUntil("parent suspended (no live runner)", func() bool {
		return !h.mgr.HasActiveLoop(parentID)
	})

	// Release the child — it completes and its result fills the pending task call.
	close(release)

	h.waitForDelivery(link.ChildID)
	h.waitUntil("parent consumed blocking child result", func() bool {
		messages := h.parentMessages(parentID)

		return countToolResultsFor(messages, "task") == 1 &&
			lastAssistantTextDTO(messages) == "parent got the child result"
	})
	h.mgr.waitIdle(parentID)

	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countToolResultsFor(msgs, "task"), "the task tool_use is filled by the child result")

	// The revived parent must make a FRESH Chat call that consumes the result and
	// produces new text — proving it did not re-suspend on the still-pending task
	// (the silent-hang the atomic delivery + in-memory append guard against).
	assert.Equal(t, "parent got the child result", lastAssistantTextDTO(msgs),
		"parent consumed the completion instead of re-suspending")

	res, err := h.mgr.Result(h.ctx, link.ChildID)
	require.NoError(t, err)
	assert.Equal(t, subagent.StateCompleted, res.State)
	assert.Equal(t, subagent.OutcomeCompleted, res.Outcome, "clean finish → completed outcome")
	assert.Contains(t, res.Output, "blocking child done")
}

func TestIntegration_CompletedForegroundChildAcceptsFollowUpInSameSession(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_INITIAL") {
			if hasUserContaining(msgs, "FOLLOW_UP") {
				return &llmwire.Response{Text: "child continuation answer"}
			}

			return &llmwire.Response{Text: "child initial answer"}
		}

		if hasUserContaining(msgs, "<subagent_completion>") {
			return &llmwire.Response{Text: "parent received continuation"}
		}

		if hasToolResultFor(msgs, "task") {
			return &llmwire.Response{Text: "parent received initial answer"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID: taskCallID, Name: "task",
			Arguments: []byte(
				`{"prompt":"CHILD_INITIAL","description":"c","subagent_type":"general"}`,
			),
		}}}
	}

	h := newHarness(t, harnessOptions{respond: respond})
	defer h.shutdown()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "start foreground child", "fake-model", nil)
	require.NoError(t, err)
	link := h.waitForChildLink(parentID)
	require.True(t, link.Blocking)
	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	require.NoError(t, h.mgr.SendToChild(h.ctx, link.ChildID, "FOLLOW_UP answer one more thing"))
	h.waitUntil("foreground continuation delivered", func() bool {
		current, linkErr := h.links.GetLink(h.ctx, link.ChildID)
		return linkErr == nil && current != nil && current.Terminal() &&
			current.DeliveredAt != 0 && current.ActivationSeq == 2
	})
	h.waitUntil("parent consumed continuation", func() bool {
		messages := h.parentMessages(parentID)

		return countSubagentCompletions(messages, link.ChildID) == 1 &&
			lastAssistantTextDTO(messages) == "parent received continuation"
	})
	h.mgr.waitIdle(parentID)

	continued, err := h.links.GetLink(h.ctx, link.ChildID)
	require.NoError(t, err)
	require.NotNil(t, continued)
	assert.False(t, continued.Blocking, "a resolved foreground task continues via async completion")
	assert.Equal(t, int64(2), continued.ActivationSeq)

	messages := h.parentMessages(parentID)
	assert.Equal(t, 1, countToolResultsFor(messages, "task"))
	assert.Equal(t, 1, countSubagentCompletions(messages, link.ChildID))
	assert.Equal(t, "parent received continuation", lastAssistantTextDTO(messages))
}

func TestIntegration_ScatterGatherBlockingTasks(t *testing.T) {
	callIDs := []string{"sg-1", "sg-2", "sg-3"}

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_TASK") {
			return &llmwire.Response{Text: "child done"}
		}

		// Once the 3 tasks have been emitted, only return final text — never
		// re-emit (would double-fork).
		if hasAssistantToolCall(msgs, "task") {
			return &llmwire.Response{Text: "all three children done"}
		}

		calls := make([]llmwire.ToolCall, len(callIDs))
		for i, id := range callIDs {
			calls[i] = llmwire.ToolCall{
				ID:        id,
				Name:      "task",
				Arguments: []byte(`{"prompt":"CHILD_TASK do it","description":"c","subagent_type":"general"}`),
			}
		}

		return &llmwire.Response{ToolCalls: calls}
	}

	h := newHarness(t, harnessOptions{respond: respond})
	defer h.shutdown()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "scatter gather", "fake-model", nil)
	require.NoError(t, err)

	// All three children are spawned, each bound to its own task call id.
	childIDs := make([]int64, 0, len(callIDs))

	for _, cid := range callIDs {
		callID := cid
		h.waitUntil("child link for "+callID, func() bool {
			link, lerr := h.links.GetLinkByTaskCallID(h.ctx, parentID, callID)
			return lerr == nil && link != nil
		})

		link, err := h.links.GetLinkByTaskCallID(h.ctx, parentID, callID)
		require.NoError(t, err)
		assert.True(t, link.Blocking)
		childIDs = append(childIDs, link.ChildID)
	}

	for _, childID := range childIDs {
		h.waitForDelivery(childID)
	}

	h.waitUntil("parent final answer", func() bool {
		return lastAssistantTextDTO(h.parentMessages(parentID)) == "all three children done"
	})
	h.mgr.waitIdle(parentID)

	// Each task tool_use is filled by its own child — exactly three results, and
	// the parent proceeds to the LLM only once all are resolved (transcript valid).
	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 3, countToolResultsFor(msgs, "task"), "each of the 3 task calls gets its own result")
}

// TestFinalizeChild_LinkReadErrorStopsShort: an unreadable link is not "this is
// not a subagent" — nothing is written, and with no parent id the log is all.
func TestFinalizeChild_LinkReadErrorStopsShort(t *testing.T) {
	h := newLedgerHarness(t)
	defer h.shutdown()

	sub := h.mgr.bus.Subscribe(h.parentID)
	defer h.mgr.bus.Unsubscribe(h.parentID, sub)

	core, logs := observer.New(zap.ErrorLevel)
	ctx := logger.ToContext(h.ctx, zap.New(core))

	h.activation.failRead = true
	h.startInboxWake()
	finalizeTestChild(ctx, t, h.mgr, h.childID)

	assert.Equal(t, 1, h.activation.attempts(), "the failed read performs no transition")

	entries := logs.FilterMessage("finalize_child").All()
	require.Len(t, entries, 1)
	assert.Equal(t, h.childID, entries[0].ContextMap()["child"])

	assert.Empty(t, drainNotifications(sub), "no parent id is known, so nothing is published")
}

// TestFinalizeChild_NoLinkIsSilent: the "no row" branch is every root session's
// normal exit — it must stay completely quiet.
func TestFinalizeChild_NoLinkIsSilent(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	defer h.shutdown()

	rec, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	sub := h.mgr.bus.Subscribe(rec.ID)
	defer h.mgr.bus.Unsubscribe(rec.ID, sub)

	core, logs := observer.New(zap.DebugLevel)
	ctx := logger.ToContext(h.ctx, zap.New(core))

	h.startInboxWake()

	finalizeTestChild(ctx, t, h.mgr, rec.ID)

	assert.Zero(t, logs.Len(), "a root session's exit logs nothing")
	assert.Empty(t, drainNotifications(sub))
}

func TestFinalizeChild_WriteFailureRemainsRecoverable(t *testing.T) {
	h := newLedgerHarness(t)
	defer h.shutdown()
	h.activation.failN = -1
	sub := h.mgr.bus.Subscribe(h.parentID)
	defer h.mgr.bus.Unsubscribe(h.parentID, sub)
	core, logs := observer.New(zap.ErrorLevel)
	ctx := logger.ToContext(h.ctx, zap.New(core))
	h.startInboxWake()
	finalizeTestChild(ctx, t, h.mgr, h.childID)
	assert.Equal(t, 1, h.activation.attempts())
	assert.Len(t, logs.FilterMessage("finalize_child").All(), 1)
	notifications := drainNotifications(sub)
	require.NotEmpty(t, notifications)
	assert.Contains(t, notifications[0].Message, "completion could not be recorded")
	running, err := h.links.ListRunningChildLinks(h.ctx)
	require.NoError(t, err)
	assert.True(t, slices.ContainsFunc(running, func(link subagent.Link) bool { return link.ChildID == h.childID }))
}

func TestIntegration_DepthCapRejected(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	defer h.shutdown()

	root, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	// depth 1: root → child A
	h.startInboxWake()
	a, err := h.mgr.Spawn(
		h.ctx,
		subagent.SpawnRequest{ParentID: root.ID, AgentType: "general", Prompt: "x"},
	)
	require.NoError(t, err)

	// depth 2: A → grandchild B (allowed — root → child → grandchild)
	h.startInboxWake()
	b, err := h.mgr.Spawn(
		h.ctx,
		subagent.SpawnRequest{ParentID: a.ChildID, AgentType: "general", Prompt: "x"},
	)
	require.NoError(t, err)

	// depth 3: B → great-grandchild — rejected as a tool error.
	h.startInboxWake()
	_, err = h.mgr.Spawn(
		h.ctx,
		subagent.SpawnRequest{ParentID: b.ChildID, AgentType: "general", Prompt: "x"},
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "nesting limit")
}

func TestIntegration_SuspendedParentHoldsNoSlot(t *testing.T) {
	release := make(chan struct{})

	h := newHarness(t, harnessOptions{respond: blockingParentRespond(func() *llmwire.Response {
		<-release

		return &llmwire.Response{Text: "child done"}
	})})
	defer func() {
		closeOnce(release)
		h.shutdown()
	}()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn blocking", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)

	// The parent suspends (loop exits, slot released); only the in-flight child
	// holds a slot. The suspended parent holds ZERO.
	h.waitUntil("parent suspended, only child holds a slot", func() bool {
		return !h.mgr.HasActiveLoop(parentID) && runnerRunningCount(h.mgr.runners) == 1
	})
	assert.Equal(t, 1, runnerChildCount(h.mgr.runners))

	closeOnce(release)
	h.waitForDelivery(link.ChildID)
}

func TestIntegration_CascadeKillsBlockingChild(t *testing.T) {
	release := make(chan struct{})

	h := newHarness(t, harnessOptions{respond: blockingParentRespond(func() *llmwire.Response {
		<-release

		return &llmwire.Response{Text: "child done"}
	})})
	defer func() {
		closeOnce(release)
		h.shutdown()
	}()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn blocking", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)
	h.waitUntil("parent suspended", func() bool { return !h.mgr.HasActiveLoop(parentID) })

	// Killing the parent must cascade-kill its in-flight blocking child.
	require.NoError(t, h.mgr.sendToSession(h.ctx, parentID, "/kill"))

	h.waitUntil("blocking child killed", func() bool {
		rec, gerr := h.store.GetSession(h.ctx, link.ChildID)
		return gerr == nil && rec.KilledAt != nil
	})

	childRec, err := h.store.GetSession(h.ctx, link.ChildID)
	require.NoError(t, err)
	assert.NotNil(t, childRec.KilledAt, "blocking descendant is killed with its parent")

	childLink, err := h.links.GetLink(h.ctx, link.ChildID)
	require.NoError(t, err)
	assert.Equal(t, subagent.StateKilled, childLink.State)
	assert.Equal(t, subagent.OutcomeKilled, childLink.Outcome, "a killed child reports the killed outcome")
}

func TestIntegration_ChildPanicMarksError(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: blockingParentRespond(func() *llmwire.Response {
		return &llmwire.Response{Text: "child model result"}
	})})
	// HTTP handler panics cannot reach the runner; the child's model commit can.
	h.mgr.build.Store = &panickingChildCommitStore{Store: h.store}
	defer h.shutdown()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn blocking", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)

	// The panicked child is marked error and its parent is unblocked.
	h.waitForDelivery(link.ChildID)
	h.waitUntil("parent consumed panicked child result", func() bool {
		return countToolResultsFor(h.parentMessages(parentID), "task") == 1
	})
	h.mgr.waitIdle(parentID)

	res, err := h.mgr.Result(h.ctx, link.ChildID)
	require.NoError(t, err)
	assert.Equal(t, subagent.StateError, res.State)
	assert.Equal(t, subagent.OutcomeError, res.Outcome, "a panicked child reports the error outcome")

	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countToolResultsFor(msgs, "task"), "parent's task call is resolved with the error")
}

func TestIntegration_StressBlockingNoDeadlock(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: blockingParentRespond(func() *llmwire.Response {
		return &llmwire.Response{Text: "child done"}
	})})
	defer h.shutdown()

	const parents = 6

	ids := make([]int64, 0, parents)

	for range parents {
		h.startInboxWake()
		id, err := h.mgr.Send(h.ctx, h.projectID, "spawn blocking", "fake-model", nil)
		require.NoError(t, err)
		ids = append(ids, id)
	}

	// Each parent spawns its child, suspends, the child completes, the parent
	// resumes — under saturation, with no deadlock.
	for _, pid := range ids {
		link := h.waitForChildLink(pid)
		h.waitForDelivery(link.ChildID)
	}

	for _, pid := range ids {
		h.waitUntil("saturated parent consumed child result", func() bool {
			messages := h.parentMessages(pid)

			return countToolResultsFor(messages, "task") == 1 && lastAssistantTextDTO(messages) == "parent done"
		})
		h.mgr.waitIdle(pid)
		msgs := h.parentMessages(pid)
		require.NoError(t, llm.ValidateToolPairing(msgs))
		assert.Equal(t, "parent done", lastAssistantTextDTO(msgs), "parent %d resumed to completion", pid)
	}

	// Caps were never exceeded; everything drained back to idle.
	assert.LessOrEqual(t, runnerChildCount(h.mgr.runners), maxChildren)
	assert.LessOrEqual(t, runnerRunningCount(h.mgr.runners), maxTotal)
}

func TestIntegration_BackgroundQueueDrains(t *testing.T) {
	release := make(chan struct{})

	ids := make([]string, 10)
	for i := range ids {
		ids[i] = fmt.Sprintf("bg-%d", i)
	}

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_TASK") {
			<-release

			return &llmwire.Response{Text: "child done"}
		}

		if hasToolResultFor(msgs, "task") {
			return &llmwire.Response{Text: "parent done"}
		}

		calls := make([]llmwire.ToolCall, len(ids))
		for i, id := range ids {
			calls[i] = llmwire.ToolCall{
				ID: id, Name: "task",
				Arguments: []byte(
					`{"prompt":"CHILD_TASK","description":"c","subagent_type":"general","background":true}`,
				),
			}
		}

		return &llmwire.Response{ToolCalls: calls}
	}

	h := newHarness(t, harnessOptions{respond: respond})
	defer func() {
		closeOnce(release)
		h.shutdown()
	}()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn many", "fake-model", nil)
	require.NoError(t, err)

	// Per-parent cap is 8: 8 children run (blocked on release), the other 2 are
	// parked in the in-memory FIFO. Every link is persisted regardless.
	h.waitUntil("8 admitted, 2 queued", func() bool {
		return runnerChildCount(h.mgr.runners) == maxPerParent && h.queueLen() == 2
	})

	// Release: the 8 finish, freeing slots; drainQueue starts the 2 parked ones.
	// All 10 must eventually complete and deliver — none dropped.
	closeOnce(release)

	for _, id := range ids {
		link := h.waitForLinkByCall(parentID, id)
		h.waitForDelivery(link.ChildID)
	}

	h.waitUntil("queue drained", func() bool { return h.queueLen() == 0 })
	assert.LessOrEqual(t, runnerChildCount(h.mgr.runners), maxChildren)
}

// A built-in explore subagent is read-only by allowlist. The daemon registers
// task/sleep/schedule onto every live session after construction, so only
// RegisterGatedTool's re-check keeps them out of the child.
func TestIntegration_ExploreChildIsDeniedControlPlaneTools(t *testing.T) {
	const exploreCallID = "task-explore-1"

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_EXPLORE") {
			return probeMissingTools(msgs, "explore", controlPlaneTools)
		}

		if hasToolResultFor(msgs, tool.IDTask) || hasUserContaining(msgs, "<subagent_completion>") {
			return &llmwire.Response{Text: "parent done"}
		}

		return &llmwire.Response{
			ToolCalls: []llmwire.ToolCall{spawnTaskCall(exploreCallID, "explore", "CHILD_EXPLORE")},
		}
	}

	h := newGatingHarness(t, nil, respond)
	defer h.shutdown()

	h.startInboxWake()
	parentID, err := h.mgr.Send(
		h.ctx, h.projectID, "spawn an explore child", "fake-model", map[string]any{"channel": "cli"},
	)
	require.NoError(t, err)

	link := h.waitForLink(parentID, exploreCallID)
	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	h.assertUnknownTools(link.ChildID, controlPlaneTools)

	offered := h.schemas.offered(link.ChildID)
	assert.Contains(t, offered, "read", "explore keeps the tools its own allowlist grants")
	assertNotOffered(t, offered, append(append([]string{}, controlPlaneTools...), configPlaneTools...))

	// Without this the child assertions would pass on a daemon that registers
	// nothing at all.
	parentOffered := h.schemas.offered(parentID)
	for _, id := range append(append([]string{}, controlPlaneTools...), configPlaneTools...) {
		assert.Contains(t, parentOffered, id, "root session must keep %q", id)
	}

	require.NoError(t, llm.ValidateToolPairing(h.parentMessages(parentID)))
}

// config_edit reaches every root session whatever its channel or manager
// attributes; children never receive it.
func TestIntegration_ConfigEditReachesEveryRootNoChild(t *testing.T) {
	tests := []struct {
		name  string
		attrs map[string]any
	}{
		{name: "channel-less root"},
		{name: "ordinary cli root", attrs: map[string]any{"channel": "cli"}},
		{
			name: "manager-owned root",
			attrs: map[string]any{
				controllerapi.SessionAttributeManagerID: "telegram-main",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newGatingHarness(t, nil, func(string, []llmwire.Message) *llmwire.Response {
				return &llmwire.Response{Text: "done"}
			})
			defer h.shutdown()

			h.startInboxWake()
			sessionID, err := h.mgr.Send(h.ctx, h.projectID, "configure", "fake-model", tt.attrs)
			require.NoError(t, err)
			h.mgr.waitIdle(sessionID)

			assert.Contains(t, h.schemas.offered(sessionID), tool.IDConfigEdit)
		})
	}

	t.Run("child", func(t *testing.T) {
		const exploreCallID = "task-explore-config-edit"

		respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
			if hasUserContaining(msgs, "CHILD_EXPLORE") {
				return probeMissingTools(msgs, "explore", configPlaneTools)
			}

			if hasToolResultFor(msgs, tool.IDTask) || hasUserContaining(msgs, "<subagent_completion>") {
				return &llmwire.Response{Text: "parent done"}
			}

			return &llmwire.Response{
				ToolCalls: []llmwire.ToolCall{spawnTaskCall(exploreCallID, "explore", "CHILD_EXPLORE")},
			}
		}

		h := newGatingHarness(t, nil, respond)
		defer h.shutdown()

		h.startInboxWake()
		parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn an explore child", "fake-model", nil)
		require.NoError(t, err)

		link := h.waitForLink(parentID, exploreCallID)
		h.waitForDelivery(link.ChildID)
		h.mgr.waitIdle(parentID)

		h.assertUnknownTools(link.ChildID, configPlaneTools)
		assertNotOffered(t, h.schemas.offered(link.ChildID), configPlaneTools)
		assert.Contains(t, h.schemas.offered(parentID), tool.IDConfigEdit)
	})
}

// Project-defined subagents are outside the built-in taxonomy: a restricted one
// is held to its own list, and even an unrestricted ("*") one stays off the
// config plane, which is guarded by parentage rather than by the allowlist.
func TestIntegration_ProjectSubagentToolGating(t *testing.T) {
	const (
		scoutCallID = "task-scout-1"
		wideCallID  = "task-wide-1"
	)

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_SCOUT") {
			return probeMissingTools(msgs, "scout", controlPlaneTools)
		}

		if hasUserContaining(msgs, "CHILD_WIDE") {
			return probeMissingTools(msgs, "wide", append(configPlaneTools, tool.IDSchedule))
		}

		if hasToolResultFor(msgs, tool.IDTask) || hasUserContaining(msgs, "<subagent_completion>") {
			return &llmwire.Response{Text: "parent done"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{
			spawnTaskCall(scoutCallID, "scout", "CHILD_SCOUT"),
			spawnTaskCall(wideCallID, "wide", "CHILD_WIDE"),
		}}
	}

	agents := map[string]string{"scout.md": scoutAgentFile, "wide.md": wideAgentFile}

	h := newGatingHarness(t, agents, respond)
	defer h.shutdown()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn project children", "fake-model", nil)
	require.NoError(t, err)

	scout := h.waitForLink(parentID, scoutCallID)
	wide := h.waitForLink(parentID, wideCallID)
	h.waitForDelivery(scout.ChildID)
	h.waitForDelivery(wide.ChildID)
	h.mgr.waitIdle(parentID)

	h.assertUnknownTools(scout.ChildID, controlPlaneTools)
	scoutOffered := h.schemas.offered(scout.ChildID)
	assert.Contains(t, scoutOffered, "read")
	assert.Contains(t, scoutOffered, "grep")
	assertNotOffered(t, scoutOffered, append([]string{"ls", "bash"}, controlPlaneTools...))
	assertNotOffered(t, scoutOffered, configPlaneTools)

	h.assertUnknownTools(wide.ChildID, append(configPlaneTools, tool.IDSchedule))
	wideOffered := h.schemas.offered(wide.ChildID)
	assert.Contains(t, wideOffered, tool.IDSleep, "subagents retain bounded suspension")
	assert.Contains(t, wideOffered, tool.IDTask, "nested subagents remain available within depth limits")
	assertNotOffered(t, wideOffered, append(configPlaneTools, tool.IDSchedule))
}

func TestIntegration_GeneralSubagentCannotScheduleButCanSleep(t *testing.T) {
	const callID = "task-general-schedule-boundary"

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_GENERAL") {
			return probeMissingTools(msgs, "general", []string{tool.IDSchedule})
		}

		if hasToolResultFor(msgs, tool.IDTask) || hasUserContaining(msgs, "<subagent_completion>") {
			return &llmwire.Response{Text: "parent done"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{
			spawnTaskCall(callID, "general", "CHILD_GENERAL"),
		}}
	}

	h := newGatingHarness(t, nil, respond)
	defer h.shutdown()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn general subagent", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForLink(parentID, callID)
	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	h.assertUnknownTools(link.ChildID, []string{tool.IDSchedule})
	offered := h.schemas.offered(link.ChildID)
	assert.NotContains(t, offered, tool.IDSchedule)
	assert.Contains(t, offered, tool.IDSleep)
	assert.Contains(t, h.schemas.offered(parentID), tool.IDSchedule)
}

// batch dispatches through a registry, so a child granted batch but not bash
// would reach bash through it unless the filtered view rebinds batch. The
// scripted child is adversarial on purpose.
func TestHarnessScenario_BatchCannotEscapeSubagentAllowlist(t *testing.T) {
	const (
		batchCallID  = "task-batcher-1"
		escapeMarker = "BATCH_ESCAPED"
		escapeFile   = "escaped.txt"
	)

	escapeCall := `{"calls":[{"tool":"bash","params":{"command":"echo ` + escapeMarker +
		` | tee ` + escapeFile + `","description":"escape the allowlist"}}]}`

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_BATCH") {
			if hasToolResultFor(msgs, tool.IDBatch) {
				return &llmwire.Response{Text: "batcher done"}
			}

			return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
				ID: "batch-escape", Name: tool.IDBatch, Arguments: []byte(escapeCall),
			}}}
		}

		if hasToolResultFor(msgs, tool.IDTask) || hasUserContaining(msgs, "<subagent_completion>") {
			return &llmwire.Response{Text: "parent done"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{
			spawnTaskCall(batchCallID, "batcher", "CHILD_BATCH"),
		}}
	}

	h := newGatingHarness(t, map[string]string{"batcher.md": batcherAgentFile}, respond)
	defer h.shutdown()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn a batching child", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForLink(parentID, batchCallID)
	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	msgs := h.parentMessages(link.ChildID)
	require.NoError(t, llm.ValidateToolPairing(msgs), "child transcript must stay provider-valid")

	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDBatch))

	batchResult := lastToolResultContent(msgs, tool.IDBatch)
	assert.Contains(t, batchResult, `unknown tool "bash"`)
	assert.NotContains(t, batchResult, escapeMarker, "the forbidden call must not have run")
	assert.NoFileExists(t, filepath.Join(h.workDir(), escapeFile))

	offered := h.schemas.offered(link.ChildID)
	assert.Contains(t, offered, tool.IDBatch, "the child really was granted batch")
	assertNotOffered(t, offered, []string{"bash", "write", "edit"})
}

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

	h := newHarness(t, harnessOptions{respond: respond})
	collector := collectEvents(h.mgr.bus.SubscribeAll())
	released := false
	defer func() {
		if !released {
			close(childRelease)
		}

		collector.stop()
		h.shutdown()
	}()

	h.startInboxWake()
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
		rec, recErr := h.store.GetSession(h.ctx, parentID)

		return recErr == nil && rec.Status == sessionstore.SessionStatusSuspended
	})
	link, err := h.links.GetLinkByTaskCallID(h.ctx, parentID, taskCallID)
	require.NoError(t, err)

	parentRec, err := h.store.GetSession(h.ctx, parentID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusSuspended, parentRec.Status,
		"the parent is durably suspended on the blocking child")
	childRec, err := h.store.GetSession(h.ctx, link.ChildID)
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

	var waitingCard string
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT content FROM session_outbox
		WHERE session_id = ? AND type = 'message_replaceable' ORDER BY id DESC LIMIT 1`, parentID).
		Scan(&waitingCard))
	assert.Contains(t, waitingCard, "🧩 Subagents · 1 foreground · 0 background")
	assert.NotContains(t, waitingCard, "🟢 Working",
		"the suspended parent must not look active while its child works")

	var waitingCards int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND type = 'message_replaceable'`, parentID).Scan(&waitingCards))
	assert.Equal(t, 1, waitingCards,
		"spawn and suspension must reuse one durable foreground waiting card")

	close(childRelease)
	released = true

	waitForVisibleMessage(t, collector, parentID, "parent collected the long child")
	drainScenarioClaims(t, "foreground_child_no_lifetime.json", newChainController(t, h))
	waitForIdleAfterMessage(t, collector, parentID, "parent collected the long child")

	link, err = h.links.GetLink(h.ctx, link.ChildID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.OutcomeCompleted, link.Outcome)

	var stoppedCard string
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT content FROM session_outbox
		WHERE session_id = ? AND source_key LIKE ? ORDER BY id DESC LIMIT 1`,
		parentID, fmt.Sprintf("progress:change:subagent:%d:%%:completed:g%%", link.ChildID)).
		Scan(&stoppedCard))
	assert.NotContains(t, stoppedCard, "Subagents",
		"terminalization must publish the card that removes the finished child")

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

func TestHarnessScenario_SubagentTextWithToolsCompletes(t *testing.T) {
	childRelease := make(chan struct{})
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "CHILD_TEXT_WITH_TOOL") {
			if hasToolResultFor(messages, "ls") {
				return &llmwire.Response{Text: "child inspection complete"}
			}

			<-childRelease

			return &llmwire.Response{
				Text: "Inspecting the project files",
				ToolCalls: []llmwire.ToolCall{{
					ID: "child-ls", Name: "ls", Arguments: []byte(`{"path":"."}`),
				}},
			}
		}

		if hasToolResultFor(messages, tool.IDTask) {
			return &llmwire.Response{Text: "child completion delivered"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:   taskCallID,
			Name: tool.IDTask,
			Arguments: []byte(
				`{"prompt":"CHILD_TEXT_WITH_TOOL","description":"scenario","subagent_type":"general"}`,
			),
		}}}
	}

	h := newHarness(t, harnessOptions{respond: respond})
	collector := collectEvents(h.mgr.bus.SubscribeAll())
	released := false
	defer func() {
		if !released {
			close(childRelease)
		}

		collector.stop()
		h.shutdown()
	}()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "run child with narrated tool call", "fake-model", nil)
	require.NoError(t, err)
	collector.waitFor(t, "parent projects the blocking child", func(events []controllerapi.SessionNotification) bool {
		return slices.ContainsFunc(events, func(event controllerapi.SessionNotification) bool {
			return event.SessionID == parentID && event.Notification.Type == sessionevent.NotifyWaiting
		})
	})
	close(childRelease)
	released = true
	waitForVisibleMessage(t, collector, parentID, "child completion delivered")
	waitForIdleAfterMessage(t, collector, parentID, "child completion delivered")

	link, err := h.links.GetLinkByTaskCallID(h.ctx, parentID, taskCallID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.OutcomeCompleted, link.Outcome)
	assert.Equal(t, 1, countToolResultsFor(transcriptOf(h, link.ChildID), "ls"))

	assertHarnessTrace(t, "subagent_text_with_tools.json", collector.snapshot(), parentID)
}

func TestHarnessScenario_ForegroundChildContinuesWithoutSleep(t *testing.T) {
	initialRelease := make(chan struct{})
	followUpRelease := make(chan struct{})
	var childID atomic.Int64

	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "CHILD_INITIAL") {
			if hasUserContaining(messages, "FOLLOW_UP") {
				<-followUpRelease
				return &llmwire.Response{Text: "child continuation answer"}
			}

			// Hold the child until the parent's suspension has projected its
			// "waiting: subagent" state — the exact event this scenario requires. An
			// instantly terminal child would race publishWaiting and legitimately
			// project nothing.
			<-initialRelease

			return &llmwire.Response{Text: "child initial answer"}
		}

		if hasUserContaining(messages, "<subagent_completion>") {
			return &llmwire.Response{Text: "continuation delivered"}
		}

		if hasUserContaining(messages, "continue the same child") {
			if hasToolResultFor(messages, "send_to_subagent") {
				return &llmwire.Response{Text: "follow-up accepted"}
			}

			id := childID.Load()
			if id == 0 {
				panic("scenario asked for follow-up before child id was captured")
			}

			return &llmwire.Response{ToolCalls: []llmwire.ToolCall{
				{
					ID:   "follow-up-call",
					Name: tool.IDSendToSubagent,
					Arguments: fmt.Appendf(nil,
						`{"id":%d,"message":"FOLLOW_UP inspect one more thing"}`,
						id,
					),
				},
				{
					ID:        "competing-sleep",
					Name:      tool.IDSleep,
					Arguments: []byte(`{"duration":"1h","reason":"wait for follow-up"}`),
				},
			}}
		}

		if hasToolResultFor(messages, "task") {
			return &llmwire.Response{Text: "initial child delivered"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:   taskCallID,
			Name: "task",
			Arguments: []byte(
				`{"prompt":"CHILD_INITIAL","description":"scenario","subagent_type":"general"}`,
			),
		}}}
	}

	h := newHarness(t, harnessOptions{respond: respond})
	collector := collectEvents(h.mgr.bus.SubscribeAll())
	defer func() {
		closeOnce(initialRelease)
		closeOnce(followUpRelease)
		collector.stop()
		h.shutdown()
	}()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "start foreground child", "fake-model", nil)
	require.NoError(t, err)
	waitForWaitKind(t, collector, parentID, sessionevent.WaitSubagent)
	close(initialRelease)
	waitForVisibleMessage(t, collector, parentID, "initial child delivered")
	waitForIdleAfterMessage(t, collector, parentID, "initial child delivered")

	link, err := h.links.GetLinkByTaskCallID(h.ctx, parentID, taskCallID)
	require.NoError(t, err)
	require.NotNil(t, link)
	require.True(t, link.Blocking, "the initial task must exercise foreground mode")
	childID.Store(link.ChildID)

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, parentID, "continue the same child"))
	waitForVisibleMessage(t, collector, parentID, "follow-up accepted")
	waitForIdleAfterMessage(t, collector, parentID, "follow-up accepted")

	close(followUpRelease)
	waitForVisibleMessage(t, collector, parentID, "continuation delivered")
	waitForIdleAfterMessage(t, collector, parentID, "continuation delivered")

	continued, err := h.links.GetLink(h.ctx, link.ChildID)
	require.NoError(t, err)
	require.NotNil(t, continued)
	assert.Equal(t, int64(2), continued.ActivationSeq)
	assert.False(t, continued.Blocking, "a foreground child continues asynchronously after its task result")
	parentMessages := h.parentMessages(parentID)
	assert.Equal(t, 1, countToolResultsFor(parentMessages, tool.IDSleep))
	assert.Contains(t, lastToolResultContent(parentMessages, tool.IDSleep),
		"sleep cannot be combined with send_to_subagent")
	schedules, err := h.schedules.ListSchedules(h.ctx, parentID)
	require.NoError(t, err)
	assert.Empty(t, schedules, "rejected sleep must not leave a competing wake-up")
	assertHarnessTrace(t, "foreground_followup_no_sleep.json", collector.snapshot(), parentID)
}

func TestHarnessScenario_BackgroundChildIsTheWakeSource(t *testing.T) {
	childRelease := make(chan struct{})

	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "CHILD_BACKGROUND") {
			<-childRelease

			return &llmwire.Response{Text: "background child answer"}
		}

		if hasUserContaining(messages, "<subagent_completion>") {
			return &llmwire.Response{Text: "background completion delivered"}
		}

		if hasToolResultFor(messages, tool.IDSleep) {
			return &llmwire.Response{Text: "background launched; yielded without sleep"}
		}

		if hasToolResultFor(messages, tool.IDTask) {
			return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
				ID:        "sleep-after-task-result",
				Name:      tool.IDSleep,
				Arguments: []byte(`{"duration":"1h","reason":"wait for background child"}`),
			}}}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:   taskCallID,
			Name: tool.IDTask,
			Arguments: []byte(
				`{"prompt":"CHILD_BACKGROUND wait for release","description":"scenario","subagent_type":"general","background":true}`,
			),
		}}}
	}

	h := newHarness(t, harnessOptions{respond: respond})
	collector := collectEvents(h.mgr.bus.SubscribeAll())
	defer func() {
		closeOnce(childRelease)
		collector.stop()
		h.shutdown()
	}()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "start background child", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	waitForVisibleMessage(t, collector, parentID, "background launched; yielded without sleep")

	link, err := h.links.GetLinkByTaskCallID(h.ctx, parentID, taskCallID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.False(t, link.Blocking)

	var runningCard string
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT content FROM session_outbox
		WHERE session_id = ? AND source_key LIKE ? ORDER BY id DESC LIMIT 1`,
		parentID, fmt.Sprintf("progress:change:subagent:%d:%%:spawned:g%%", link.ChildID)).
		Scan(&runningCard))
	assert.Contains(t, runningCard, "🧩 Subagents · 0 foreground · 1 background")

	parentMessages := h.parentMessages(parentID)
	assert.Equal(t, 1, countToolResultsFor(parentMessages, tool.IDSleep))
	assert.Contains(t, lastToolResultContent(parentMessages, tool.IDSleep),
		"result arrives automatically in a later turn")
	schedules, err := h.schedules.ListSchedules(h.ctx, parentID)
	require.NoError(t, err)
	assert.Empty(t, schedules, "pending child must remain the sole wake source")
	h.mgr.waitIdle(parentID)

	close(childRelease)
	waitForVisibleMessage(t, collector, parentID, "background completion delivered")
	drainScenarioClaims(t, "background_child_no_sleep.json", newChainController(t, h))
	waitForIdleAfterMessage(t, collector, parentID, "background completion delivered")

	var stoppedCard string
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT content FROM session_outbox
		WHERE session_id = ? AND source_key LIKE ? ORDER BY id DESC LIMIT 1`,
		parentID, fmt.Sprintf("progress:change:subagent:%d:%%:completed:g%%", link.ChildID)).
		Scan(&stoppedCard))
	assert.NotContains(t, stoppedCard, "Subagents")

	assertHarnessTrace(t, "background_child_no_sleep.json", collector.snapshot(), parentID)
}

func TestHarnessScenario_BackgroundFinalResponseResumesOnCompletion(t *testing.T) {
	childRelease := make(chan struct{})
	var rootCalls atomic.Int64

	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "CHILD_CANARY") {
			<-childRelease

			return &llmwire.Response{Text: "canary child complete"}
		}
		rootCalls.Add(1)

		if hasUserContaining(messages, "<subagent_completion>") {
			return &llmwire.Response{Text: "completion after wait"}
		}

		if hasToolResultFor(messages, tool.IDTask) {
			return &llmwire.Response{Text: "child still running"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID: taskCallID, Name: tool.IDTask,
			Arguments: []byte(
				`{"prompt":"CHILD_CANARY","description":"scenario","subagent_type":"general","background":true}`,
			),
		}}}
	}

	h := newHarness(t, harnessOptions{respond: respond})
	collector := collectEvents(h.mgr.bus.SubscribeAll())
	defer func() {
		closeOnce(childRelease)
		collector.stop()
		h.shutdown()
	}()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "start canary child", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	waitForVisibleMessage(t, collector, parentID, "child still running")

	parentMessages := h.parentMessages(parentID)
	require.True(t, slices.ContainsFunc(parentMessages, func(message llmwire.Message) bool {
		return message.Role == llmwire.RoleAssistant &&
			message.Content == "child still running" && len(message.ToolCalls) == 0
	}))

	var outputType, output string
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT type, content FROM session_outbox
		WHERE session_id = ? AND content LIKE '%child still running%' ORDER BY id DESC LIMIT 1`, parentID).
		Scan(&outputType, &output))
	assert.Equal(t, string(sessionstore.OutputMessagePersistent), outputType)
	// The yield final carries the background badge title ahead of the model text.
	assert.True(t, strings.HasPrefix(output, "🟣 Background\n\n"), "yield card: %q", output)
	assert.Contains(t, output, "child still running")

	// Settle the root's runner before releasing the child: the golden trace pins
	// the resume as a fresh session loop, which requires the old runner to be gone.
	h.mgr.waitIdle(parentID)
	close(childRelease)
	waitForVisibleMessage(t, collector, parentID, "completion after wait")
	drainScenarioClaims(t, "background_wait_canary.json", newChainController(t, h))
	waitForIdleAfterMessage(t, collector, parentID, "completion after wait")
	// The completion wake's candidate and its confirmation plus the earlier
	// task turn and its yield: four root calls under the two-phase check.
	assert.Equal(t, int64(4), rootCalls.Load())
	assertHarnessTrace(t, "background_wait_canary.json", collector.snapshot(), parentID)
}

func TestHarnessScenario_ForegroundScatterGatherProjectsShrinkingAllWaitSet(t *testing.T) {
	callIDs := []string{"wait-1", "wait-2", "wait-3"}
	releases := []chan struct{}{make(chan struct{}), make(chan struct{}), make(chan struct{})}

	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		for i := range releases {
			if hasUserContaining(messages, fmt.Sprintf("CHILD_WAIT_%d", i+1)) {
				<-releases[i]

				return &llmwire.Response{Text: fmt.Sprintf("child %d done", i+1)}
			}
		}

		if hasAssistantToolCall(messages, tool.IDTask) {
			return &llmwire.Response{Text: "all children delivered"}
		}

		calls := make([]llmwire.ToolCall, len(callIDs))
		for i, callID := range callIDs {
			calls[i] = llmwire.ToolCall{
				ID:   callID,
				Name: tool.IDTask,
				Arguments: fmt.Appendf(nil,
					`{"prompt":"CHILD_WAIT_%d","description":"wait child","subagent_type":"general"}`,
					i+1,
				),
			}
		}

		return &llmwire.Response{ToolCalls: calls}
	}

	h := newHarness(t, harnessOptions{respond: respond})
	collector := collectEvents(h.mgr.bus.SubscribeAll())
	defer func() {
		for _, release := range releases {
			closeOnce(release)
		}
		collector.stop()
		h.shutdown()
	}()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "scatter gather", "fake-model", nil)
	require.NoError(t, err)

	releaseOf := map[int64]chan struct{}{}
	childIDs := make([]int64, 0, len(callIDs))
	for i, callID := range callIDs {
		h.waitUntil("child link for "+callID, func() bool {
			link, linkErr := h.links.GetLinkByTaskCallID(h.ctx, parentID, callID)

			return linkErr == nil && link != nil
		})
		link, linkErr := h.links.GetLinkByTaskCallID(h.ctx, parentID, callID)
		require.NoError(t, linkErr)
		releaseOf[link.ChildID] = releases[i]
		childIDs = append(childIDs, link.ChildID)
	}
	slices.Sort(childIDs)

	waitForSubagentSet(t, collector, parentID, childIDs)
	assert.Zero(t, countPublishedMessage(collector.snapshot(), parentID, "all children delivered"),
		"the model must not run while any foreground child is pending")

	// Release in child-id order: spawn order across concurrent task calls is not
	// fixed, so only this makes the recorded shrink sequence reproducible.
	for i, childID := range childIDs {
		close(releaseOf[childID])
		h.waitForDelivery(childID)

		if i < len(childIDs)-1 {
			waitForSubagentSet(t, collector, parentID, childIDs[i+1:])
		}
	}

	waitForVisibleMessage(t, collector, parentID, "all children delivered")
	waitForIdleAfterMessage(t, collector, parentID, "all children delivered")

	for _, event := range collector.snapshot() {
		if event.SessionID != parentID || event.Notification.Type != sessionevent.NotifyWaiting {
			continue
		}

		for _, item := range event.Notification.Waiting {
			assert.Equal(t, sessionevent.WaitSubagent, item.Kind)
			assert.Nil(t, item.WakeAt)
		}
	}

	assertHarnessTrace(t, "scatter_gather_shrinking_wait_set.json", collector.snapshot(), parentID)
}

func TestIntegration_BackgroundSubagentCompletes(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	defer h.shutdown()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "SPAWN_CHILD please", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)
	assert.Equal(t, parentID, link.ParentID)
	assert.Equal(t, taskCallID, link.TaskCallID)
	assert.False(t, link.Blocking)

	// Regression guard for the cascade-kill change (#12): an *idle* (not killed)
	// parent must still survive its background child and be revived by the child's
	// completion — only a deliberately killed tree drops background descendants.
	h.waitForDelivery(link.ChildID)
	h.waitForParentCompletions(parentID, link.ChildID, 1)

	// Parent transcript is a valid tool_use/tool_result pairing.
	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs), "parent transcript must be transcript-valid")

	// Exactly one completion record for the child.
	assert.Equal(t, 1, countSubagentCompletions(msgs, link.ChildID), "exactly one completion record")

	// get_subagent_result returns completed + output, and the auto-delivered
	// completion shows the SAME formatted string as get_subagent_result.
	res, err := h.mgr.Result(h.ctx, link.ChildID)
	require.NoError(t, err)
	assert.True(t, res.Terminal)
	assert.Equal(t, subagent.StateCompleted, res.State)
	assert.Equal(t, subagent.OutcomeCompleted, res.Outcome)
	assert.Contains(t, res.Output, "child finished")
	completion := lastSubagentCompletion(msgs, link.ChildID)
	assert.Contains(t, completion, "outcome: completed")
	assert.Contains(t, completion, "result:\nchild finished: 42")
}

// A model-authored task+sleep batch is not a valid join: both tools execute
// concurrently, so the sleep cannot order the child and creates a competing wake
// protocol. The harness launches the background child but rejects sleep before
// it stages a timer; child completion remains the sole wake source.
func TestIntegration_BackgroundTaskRejectsCompetingSleepProtocol(t *testing.T) {
	childRelease := make(chan struct{})
	const sleepCallID = "sleep-call-125"

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_TASK") {
			<-childRelease
			return &llmwire.Response{Text: "child finished while parent slept"}
		}

		if hasUserContaining(msgs, "<subagent_completion>") {
			return &llmwire.Response{Text: "child completion handled"}
		}

		if hasToolResultFor(msgs, tool.IDSleep) {
			return &llmwire.Response{Text: "background launched; yielding without sleep"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{
			{
				ID:   taskCallID,
				Name: tool.IDTask,
				Arguments: []byte(
					`{"prompt":"CHILD_TASK wait for release","description":"child work","subagent_type":"general","background":true}`,
				),
			},
			{
				ID:        sleepCallID,
				Name:      tool.IDSleep,
				Arguments: []byte(`{"duration":"1h","reason":"wait for child"}`),
			},
		}}
	}

	h := newHarness(t, harnessOptions{respond: respond})
	defer h.shutdown()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn and wait", "fake-model", nil)
	require.NoError(t, err)
	link := h.waitForChildLink(parentID)

	h.waitUntil("parent yields without sleeping", func() bool { return !h.mgr.HasActiveLoop(parentID) })

	schedules, err := h.schedules.ListSchedules(h.ctx, parentID)
	require.NoError(t, err)
	require.Empty(t, schedules, "rejected sleep must stage no timer")

	close(childRelease)
	h.waitForDelivery(link.ChildID)
	h.waitForParentCompletions(parentID, link.ChildID, 1)
	h.mgr.waitIdle(parentID)

	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs), "the whole transcript must remain provider-valid")
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDSleep))
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDSleep), "the rejected call gets one error result")
	assert.Equal(t, 1, countSubagentCompletions(msgs, link.ChildID), "the child completion is delivered exactly once")

	var sleepResult *llmwire.Message
	for i := range msgs {
		if msgs[i].Role == llmwire.RoleTool && msgs[i].ToolName == tool.IDSleep {
			sleepResult = &msgs[i]
			break
		}
	}
	require.NotNil(t, sleepResult)
	assert.Equal(t, sleepCallID, sleepResult.ToolCallID, "the result must target the model's original call id")
	assert.Contains(t, sleepResult.Content, "sleep cannot be combined with task")

	schedules, err = h.schedules.ListSchedules(h.ctx, parentID)
	require.NoError(t, err)
	assert.Empty(t, schedules)
}

func TestIntegration_SendToSubagentReNotifies(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	defer h.shutdown()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "SPAWN_CHILD please", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)
	h.waitForDelivery(link.ChildID)
	h.waitForParentCompletions(parentID, link.ChildID, 1)

	// Re-engage the finished child with follow-up work.
	require.NoError(t, h.mgr.SendToChild(h.ctx, link.ChildID, "MORE_WORK for the CHILD_TASK"))

	// A new completion is owed and re-delivered.
	h.waitForDelivery(link.ChildID)
	h.waitForParentCompletions(parentID, link.ChildID, 2)

	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.GreaterOrEqual(
		t,
		countSubagentCompletions(msgs, link.ChildID),
		2,
		"follow-up produces a second completion record",
	)
}

// TestLedgerFailure_SpawnRefusesInsteadOfDegrading is the summary gate: with the
// ledger unreadable the spawn must be REFUSED, not quietly granted at depth 1,
// outside the parent quota and without a wall-clock timeout.
func TestLedgerFailure_SpawnRefusesInsteadOfDegrading(t *testing.T) {
	var flaky *flakyLinkStore

	h := newHarness(t, harnessOptions{respond: trivialRespond, links: func(inner subagent.Store) subagent.Store {
		flaky = newFlakyLinkStore(inner)
		return flaky
	}})
	defer h.shutdown()

	root, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	// Healthy store: the same request succeeds — this gate must not simply refuse
	// everything.
	h.startInboxWake()
	ok, err := h.mgr.Spawn(h.ctx, subagent.SpawnRequest{ParentID: root.ID, AgentType: "general", Prompt: "x"})
	require.NoError(t, err)
	require.NotZero(t, ok.ChildID)
	h.waitForDelivery(ok.ChildID)
	h.mgr.waitIdle(ok.ChildID)
	h.waitUntil("healthy child runner removed", func() bool { return runnerCount(h.mgr.runners) == 0 })

	loopsBefore := runnerCount(h.mgr.runners)

	childrenBefore := runnerChildCount(h.mgr.runners)

	flaky.failGetLink(1, 0)

	// Assert on the returned error, not on HasActiveLoop: the spawn dies in
	// childDepth before the child session exists, so there is no id to look up.
	h.startInboxWake()
	res, err := h.mgr.Spawn(h.ctx, subagent.SpawnRequest{ParentID: root.ID, AgentType: "general", Prompt: "x"})
	require.Error(t, err)
	assert.Equal(t, subagent.ChildResult{}, res)

	loopsAfter := runnerCount(h.mgr.runners)

	assert.Equal(t, loopsBefore, loopsAfter, "no runner was started")
	assert.Equal(t, childrenBefore, runnerChildCount(h.mgr.runners), "no child slot was taken")
}

func TestResponseIntegrity_ReusedChildReportsCurrentErrorInsteadOfPriorAnswer(t *testing.T) {
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "CHILD_INTEGRITY") {
			if hasUserContaining(messages, "FAIL_CURRENT_ROUND") {
				return &llmwire.Response{Text: "rejected child partial", FinishType: llmwire.FinishUnknown}
			}
			return &llmwire.Response{Text: "prior child answer"}
		}
		if hasUserContaining(messages, "<subagent_completion>") {
			return &llmwire.Response{Text: "parent consumed child outcome"}
		}

		return taskResponse("CHILD_INTEGRITY", "integrity")
	}

	h := newHarness(t, harnessOptions{respond: respond})
	defer h.shutdown()
	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "start integrity child", "fake-model", nil)
	require.NoError(t, err)
	link := h.waitForChildLink(parentID)
	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	require.NoError(t, h.mgr.SendToChild(h.ctx, link.ChildID, "FAIL_CURRENT_ROUND"))
	h.waitUntil("integrity-error continuation delivered", func() bool {
		current, linkErr := h.links.GetLink(h.ctx, link.ChildID)
		return linkErr == nil && current != nil && current.Terminal() && current.DeliveredAt != 0 &&
			current.ActivationSeq == 2
	})
	current, err := h.links.GetLink(h.ctx, link.ChildID)
	require.NoError(t, err)
	assert.Equal(t, subagent.OutcomeError, current.Outcome)
	assert.Equal(t, sessionstore.UnknownFinishTerminalError, current.Result)
	assert.NotContains(t, current.Result, "prior child answer")
}

func TestResponseIntegrity_IncompleteChildResponseCannotBecomeCompletion(t *testing.T) {
	cases := []struct {
		name     string
		response llmwire.Response
	}{
		{
			name: "empty tool finish",
			response: llmwire.Response{
				Text: "hidden child text", FinishType: llmwire.FinishToolCalls,
			},
		},
		{
			name: "whitespace stop",
			response: llmwire.Response{
				Text: " \n\t ", FinishType: llmwire.FinishStop,
			},
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			_, current := runIncompleteChildResponse(t, &tt.response)
			assert.NotContains(t, current.Result, tt.response.Text)
			assert.Contains(
				t,
				current.Result,
				sessionstore.EmptyStopTerminalNotice(sessionstore.EmptyStopTerminalStreak),
			)
			assert.Equal(t, subagent.OutcomeCompleted, current.Outcome)
		})
	}
}

func TestResponseIntegrity_MissingTerminalRejectionNeverReusesOlderAnswer(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	defer h.shutdown()
	parent, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID, err := func() (int64, error) {
		var id int64
		err := h.store.WithTx(h.ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				h.ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      h.projectID,
					ParentID:       parent.ID,
					RootID:         parent.ID,
					AgentType:      "general",
					Model:          "fake-model",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, err)
	require.NoError(t, seedChildLink(h.ctx, h.store, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "missing-rejection",
	}))
	_, err = h.store.Commit(h.ctx, sessionstore.Commit{SessionID: childID, Messages: []*transcript.Message{{
		Role: llmwire.RoleAssistant, Content: "older accepted answer", FinishType: llmwire.FinishStop,
	}}})
	require.NoError(t, err)
	require.NoError(t, func() error {
		iteration := 2
		status := sessionstore.SessionStatusError
		_, err := h.store.Commit(
			h.ctx,
			sessionstore.Commit{
				SessionID: childID,
				State:     sessionstore.StatePatch{Iteration: &iteration, Status: &status},
			},
		)
		return err
	}())

	finalizeTestChild(h.ctx, t, h.mgr, childID)
	link, err := h.links.GetLink(h.ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, subagent.OutcomeError, link.Outcome)
	assert.NotContains(t, link.Result, "older accepted answer")
	assert.Contains(t, link.Result, "crashed after 2 iterations")
}

// TestSpawnSettlesTheChildEffortOnTheChildModel drives a spawn onto a model whose
// effort vocabulary differs from the parent's. The parent's level is meaningless
// there, so the child must start on its own model's default — that is what its
// record has to carry, because the record is all the child's run reads.
func TestSpawnSettlesTheChildEffortOnTheChildModel(t *testing.T) {
	provider := newSpawnEffortProvider(t)
	h := newHarness(
		t,
		harnessOptions{
			configure: withSpawnEffortModels(provider.url),
			clientFor: configuredClient(withSpawnEffortModels(provider.url)),
		},
	)

	defer h.shutdown()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "parent work", "parent-model", nil)
	require.NoError(t, err)
	// The two-phase check spends a hidden candidate and a confirmation, both
	// "done" from the stub, so one visible turn is two assistant rows.
	h.waitUntil("parent answered", func() bool {
		return countAssistantReplies(h.parentMessages(parentID)) == 2
	})
	h.mgr.waitIdle(parentID)

	require.NoError(t, h.mgr.SetModel(h.ctx, parentID, "parent-model", "high"))

	h.startInboxWake()
	child, err := h.mgr.Spawn(h.ctx, subagent.SpawnRequest{
		ParentID:  parentID,
		AgentType: "general",
		Model:     "child-model",
		Prompt:    "child work",
	})
	require.NoError(t, err)
	h.waitForDelivery(child.ChildID)

	rec, err := h.store.GetSession(h.ctx, child.ChildID)
	require.NoError(t, err)
	assert.Equal(t, "low", rec.ReasoningLevel,
		"the parent's level is not a level the child model offers, so the child model's default wins")

	assert.Equal(t, "low", provider.effortFor("child-model"),
		"the child must ask the provider for the level its own model defaults to")
}

func TestStopRejectsSpawnQueuedBehindDurableBoundary(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	defer h.shutdown()

	root, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	store := &stoppingGateStore{
		Store:     h.mgr.store,
		sessionID: root.ID,
		written:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	h.mgr.store = store
	t.Cleanup(func() {
		store.releaseOnce.Do(func() { close(store.release) })
		h.mgr.store = store.Store
	})

	stopDone := make(chan error, 1)
	go func() { stopDone <- h.mgr.sendToSession(context.Background(), root.ID, "/stop") }()

	requireBarrierSignal(t, store.written, "stop did not durably mark the root stopping")
	record, err := store.GetSession(h.ctx, root.ID)
	require.NoError(t, err)
	require.Equal(t, sessionstore.SessionStatusStopping, record.Status)

	spawnDone := make(chan error, 1)
	go func() {
		h.startInboxWake()
		_, spawnErr := h.mgr.Spawn(h.ctx, subagent.SpawnRequest{ParentID: root.ID, AgentType: "general", Prompt: "x"})
		spawnDone <- spawnErr
	}()

	select {
	case spawnErr := <-spawnDone:
		t.Fatalf("spawn crossed the active stop boundary: %v", spawnErr)
	default:
	}

	store.releaseOnce.Do(func() { close(store.release) })
	require.NoError(t, <-stopDone)
	require.ErrorContains(t, <-spawnDone, "not found or already terminal")

	records, err := store.ListAllSessions(h.ctx)
	require.NoError(t, err)
	assert.Len(t, records, 1, "the queued spawn must not create a child or link")
}

func TestSpawnRejectsStoppedParent(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	defer h.shutdown()

	root, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	require.NoError(t, h.mgr.sendToSession(context.Background(), root.ID, "/stop"))

	h.startInboxWake()
	_, err = h.mgr.Spawn(h.ctx, subagent.SpawnRequest{ParentID: root.ID, AgentType: "general", Prompt: "x"})
	require.ErrorContains(t, err, "not found or already terminal")
}

// TestChildDepth_ReadErrorCancelsSpawn: a ledger read failure must not read as
// "parent has no link" — that would reset the nesting depth to 1 and let a spawn
// through that the cap should have rejected.
func TestChildDepth_ReadErrorCancelsSpawn(t *testing.T) {
	var flaky *flakyLinkStore

	h := newHarness(t, harnessOptions{respond: trivialRespond, links: func(inner subagent.Store) subagent.Store {
		flaky = newFlakyLinkStore(inner)
		return flaky
	}})
	defer h.shutdown()

	root, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	flaky.failGetLink(1, 0)

	h.startInboxWake()
	_, err = h.mgr.Spawn(h.ctx, subagent.SpawnRequest{ParentID: root.ID, AgentType: "general", Prompt: "x"})
	require.ErrorIs(t, err, errLinkRead)
}

// TestChildDepth_NoLinkKeepsDepthOne: the "no row" branch is the normal path for
// every root session and must stay untouched.
func TestChildDepth_NoLinkKeepsDepthOne(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	defer h.shutdown()

	root, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	depth, err := h.mgr.childDepth(h.ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, depth)

	h.startInboxWake()
	child, err := h.mgr.Spawn(h.ctx, subagent.SpawnRequest{ParentID: root.ID, AgentType: "general", Prompt: "x"})
	require.NoError(t, err)

	link, err := h.links.GetLink(h.ctx, child.ChildID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, 1, link.Depth)
}

// A child that stops with a full candidate, then confirms with a terse ack,
// hands its parent the candidate text as the result — not the ack. This is the
// real deriveOutcome consumer of the confirmed-answer pointer.
func TestSubagentResult_CarriesCandidateAnswerNotAck(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	defer h.shutdown()

	ctx := h.ctx
	parent, err := h.store.CreateSession(ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID, err := func() (int64, error) {
		var id int64
		err := h.store.WithTx(ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      h.projectID,
					ParentID:       parent.ID,
					RootID:         parent.ID,
					AgentType:      "general",
					Model:          "fake-model",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, err)
	require.NoError(t, seedChildLink(ctx, h.store, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "cand",
	}))

	seedChildCandidateConfirm(t, h, childID)

	h.startInboxWake()

	finalizeTestChild(ctx, t, h.mgr, childID)

	link, err := h.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.OutcomeCompleted, link.Outcome)
	assert.Contains(t, link.Result, "the full child answer",
		"the child result is the candidate answer")
	assert.NotContains(t, link.Result, "why I am stopping",
		"the ack never becomes the child result")
}

// A stale pointer never turns an errored child into a completed answer: the
// error outcome keeps precedence over the confirmed-answer pointer.
func TestSubagentResult_ErrorBeatsStalePointer(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	defer h.shutdown()

	ctx := h.ctx
	parent, err := h.store.CreateSession(ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID, err := func() (int64, error) {
		var id int64
		err := h.store.WithTx(ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      h.projectID,
					ParentID:       parent.ID,
					RootID:         parent.ID,
					AgentType:      "general",
					Model:          "fake-model",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, err)
	require.NoError(t, seedChildLink(ctx, h.store, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "stale",
	}))

	seedChildCandidateConfirm(t, h, childID)

	// The child then errors (max iterations persists error status).
	require.NoError(t, h.store.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusError))
	_, err = h.store.Commit(ctx, sessionstore.Commit{SessionID: childID, Messages: []*transcript.Message{{
		Role: "assistant", ToolCalls: json.RawMessage(`[{"id":"x","name":"bash","arguments":{}}]`),
	}}})
	require.NoError(t, err)

	h.startInboxWake()

	finalizeTestChild(ctx, t, h.mgr, childID)

	link, err := h.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.OutcomeIncomplete, link.Outcome,
		"the pointer must not mask a failed child: error precedence gives the incomplete outcome")
	assert.Contains(t, link.Result, "without a final answer", "the failure result stands")
	assert.NotContains(t, link.Result, "the full child answer")
}

// TestFinalizeChild_IncompleteWhenNoFinalAnswer: a child that ran out of
// iterations with its last message a tool call (no final answer) terminalizes as
// `incomplete`, not a silent `completed`, and the parent sees that explicitly.
func TestFinalizeChild_IncompleteWhenNoFinalAnswer(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	defer h.shutdown()

	ctx := h.ctx

	parent, err := h.store.CreateSession(ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)

	childID, err := func() (int64, error) {
		var id int64
		err := h.store.WithTx(ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      h.projectID,
					ParentID:       parent.ID,
					RootID:         parent.ID,
					AgentType:      "general",
					Model:          "fake-model",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, err)
	require.NoError(t, seedChildLink(ctx, h.store, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "bg",
	}))

	// The child's last message is a tool call — it stopped mid-tool / hit its cap.
	toolCalls, err := json.Marshal([]llmwire.ToolCall{{ID: "x", Name: "bash", Arguments: []byte(`{}`)}})
	require.NoError(t, err)
	_, err = h.store.Commit(ctx, sessionstore.Commit{SessionID: childID, Messages: []*transcript.Message{{
		Role: llmwire.RoleAssistant, ToolCalls: toolCalls,
	}}})
	require.NoError(t, err)
	// Max-iterations persists status "error" with errored == false.
	require.NoError(t, func() error {
		iteration := 12
		status := sessionstore.SessionStatusError
		_, err := h.store.Commit(
			ctx,
			sessionstore.Commit{
				SessionID: childID,
				State:     sessionstore.StatePatch{Iteration: &iteration, Status: &status},
			},
		)
		return err
	}())

	h.startInboxWake()

	finalizeTestChild(ctx, t, h.mgr, childID)

	link, err := h.links.GetLink(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, subagent.OutcomeIncomplete, link.Outcome, "no final answer → incomplete")
	assert.Contains(t, link.Result, "without a final answer")
	assert.Contains(t, link.Result, "12", "result note carries the iteration count")
	assert.Equal(t, subagent.StateError, link.State, "max-iterations keeps the state=error lifecycle value")

	// The parent receives the explicit incomplete outcome, never a masked completed.
	h.waitForDelivery(childID)
	h.mgr.waitIdle(parent.ID)
	assert.Contains(t, lastSubagentCompletion(h.parentMessages(parent.ID), childID), "outcome: incomplete")
}

// Activation 1 confirms (pointer set), a re-activation ends in a
// background-yield final: deriveOutcome must return the yield text, never the
// stale confirmed answer — the pointer is cleared by external input, and
// without it the last-assistant-text fallback stands.
func TestSubagentResult_BackgroundYieldFallsBackToYieldText(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	defer h.shutdown()

	ctx := h.ctx
	parent, err := h.store.CreateSession(ctx, h.projectID, "fake-model", "", nil)
	require.NoError(t, err)
	childID, err := func() (int64, error) {
		var id int64
		err := h.store.WithTx(ctx, func(tx *sql.Tx) error {
			var err error
			id, err = sessionstore.CreateSubagentSessionTx(
				ctx,
				tx,
				sessionstore.CreateSubagentSession{
					ProjectID:      h.projectID,
					ParentID:       parent.ID,
					RootID:         parent.ID,
					AgentType:      "general",
					Model:          "fake-model",
					ReasoningLevel: "",
				},
			)
			return err
		})
		return id, err
	}()
	require.NoError(t, err)
	require.NoError(t, seedChildLink(ctx, h.store, subagent.Link{
		ParentID: parent.ID, ChildID: childID, TaskCallID: "yield",
	}))

	// Activation 1: candidate + confirm leaves the durable pointer.
	seedChildCandidateConfirm(t, h, childID)

	// The re-activation's external model-visible input clears the stale
	// pointer alongside the check — clearing happens at promotion, not at
	// enqueue.
	input, err := h.store.Enqueue(
		ctx,
		sessionstore.Input{
			SessionID:  childID,
			Source:     sessionstore.InputSourceProcess,
			Content:    "wake",
			Attributes: nil,
		},
	)
	require.NoError(t, err)
	_, err = h.store.Commit(
		ctx,
		sessionstore.Commit{
			SessionID: input.Input.SessionID,
			Accept: []sessionstore.Accept{
				{
					InputID:    input.Input.ID,
					State:      sessionstore.InputStateAccepted,
					Content:    "wake",
					LinkRef:    -1,
					ModelBound: true,
				},
			},
		},
	)
	require.NoError(t, err)

	// Activation 2 ends in a plain yield final (no pending check).
	_, err = h.store.Commit(ctx, sessionstore.Commit{SessionID: childID, Messages: []*transcript.Message{{
		Role: "assistant", Content: "fresh yield text",
	}}})
	require.NoError(t, err)
	require.NoError(t, h.store.UpdateSessionStatus(ctx, childID, sessionstore.SessionStatusCompleted))

	var pointer int64
	require.NoError(t, h.db.QueryRowContext(ctx,
		`SELECT COALESCE(completion_check_confirmed_answer_id, 0) FROM sessions WHERE id = ?`,
		childID).Scan(&pointer))
	assert.Zero(t, pointer, "external input clears the stale pointer")

	h.startInboxWake()

	finalizeTestChild(ctx, t, h.mgr, childID)

	link, err := h.links.GetLink(ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.OutcomeCompleted, link.Outcome)
	assert.Contains(t, link.Result, "fresh yield text", "the fallback is the real final text")
	assert.NotContains(t, link.Result, "the full child answer",
		"the stale confirmed answer must not resurface as the result")
	_ = sessionstore.SessionStatusCompleted
}
