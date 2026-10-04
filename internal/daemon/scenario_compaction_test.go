package daemon

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

// TestScenario_AutomaticCompactionRunsInsideTheDaemon drives the threshold path
// (not /compact) end to end on a real daemon: the provider reports usage over the
// cutoff, the sanctioned compaction point fires, and the session keeps running on
// the rebuilt transcript. The follow-up round exists because the verbatim tail is
// never empty: two raw groups give the split something to summarize and something
// to keep.
func TestScenario_AutomaticCompactionRunsInsideTheDaemon(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		switch {
		case isCompactionPrompt(msgs):
			return &llmwire.Response{Text: autoCompactionBrief}
		case hasUserContaining(msgs, contextSummaryPrefix):
			return &llmwire.Response{Text: "work complete"}
		case hasToolResultFor(msgs, "read"):
			return &llmwire.Response{Text: "uncompacted fallback"}
		case hasToolResultFor(msgs, "ls"):
			return &llmwire.Response{
				ToolCalls: []llmwire.ToolCall{{
					ID:        "read-followup",
					Name:      "read",
					Arguments: []byte(`{"file_path":"go.mod"}`),
				}},
			}
		default:
			return scriptedToolCall("ls", `{"path":"."}`)
		}
	}

	h := newHarness(t, harnessOptions{respond: respond})
	defer h.shutdown()

	events := collectEvents(h.mgr.bus.SubscribeAll())
	defer events.stop()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "start the scripted work", "fake-model", nil)
	require.NoError(t, err)

	h.waitUntil("session finishes on the compacted transcript", func() bool {
		return !h.mgr.HasActiveLoop(parentID) &&
			lastAssistantTextDTO(h.parentMessages(parentID)) == "work complete"
	})

	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs), "the rebuilt transcript must stay provider-valid")

	// header → marked summary → the verbatim tail (the follow-up round) → the
	// work the loop continued on the rebuilt transcript.
	summaryAt := indexOfSummary(msgs)
	require.Positive(t, summaryAt, "the header survives ahead of the summary")
	require.Equal(t, 1, countSummaryRows(msgs), "exactly one summary row")
	assert.Equal(t, llmwire.RoleUser, msgs[0].Role)
	assert.Contains(t, msgs[0].Content, "start the scripted work")
	assert.Contains(t, msgs[summaryAt].Content, autoCompactionBrief)
	require.Greater(t, len(msgs), summaryAt+2, "a verbatim tail survives behind the summary")
	assert.Equal(t, "work complete", msgs[len(msgs)-1].Content, "the loop continued on the checkpoint")

	// Everything the summary replaced is gone, including the settled tool pair.
	assert.False(t, hasAssistantToolCall(msgs, "ls"), "the summarized tool_use is gone")
	assert.Zero(t, countToolResultsFor(msgs, "ls"))

	trace := events.snapshot()
	for i, e := range trace {
		t.Logf("TRACE %d: sess=%d type=%s msg=%q", i, e.SessionID, e.Notification.Type, e.Notification.Message)
	}
	assert.Equal(t, 1, countPublishedMessage(trace, parentID, compactionStartNotice))
	assert.Equal(t, 1, countPublishedMessage(trace, parentID, compactionDoneNotice))
	assert.Zero(t, countPublishedMessage(trace, parentID, "❌ Compaction failed"))
}

// TestScenario_AutoCompactionWhileABackgroundChildIsInFlight pins the ordering
// contract the compaction guard does NOT cover: a background child is neither a
// pending external call nor pending work, so automatic compaction runs while it
// is out. Its completion must still reach the parent exactly once, paired, on a
// transcript the summary already rebuilt.
func TestScenario_AutoCompactionWhileABackgroundChildIsInFlight(t *testing.T) {
	childRelease := make(chan struct{})
	compactionEntered := make(chan struct{})
	compactionRelease := make(chan struct{})

	var enteredOnce sync.Once

	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		switch {
		case isCompactionPrompt(msgs):
			// The child is released only once compaction is provably in flight, so
			// its completion is produced while the parent holds a stale snapshot.
			enteredOnce.Do(func() { close(compactionEntered) })
			<-compactionRelease

			return &llmwire.Response{Text: autoCompactionBrief}
		case hasUserContaining(msgs, "CHILD_TASK"):
			<-childRelease

			return &llmwire.Response{Text: "background child done: 7"}
		case hasUserContaining(msgs, "<subagent_completion>"):
			return &llmwire.Response{Text: "child completion handled"}
		case hasUserContaining(msgs, contextSummaryPrefix):
			return &llmwire.Response{Text: "parent continued after compaction"}
		case hasToolResultFor(msgs, tool.IDTask):
			// The follow-up round is deliberately unmeasured: it only exists so
			// the raw range holds two groups when the threshold fires — the
			// newest group stays verbatim in the tail, the launch pair (the
			// thing this scenario watches mid-flight) is what gets summarized.
			return &llmwire.Response{
				ToolCalls: []llmwire.ToolCall{{
					ID:        "ls-followup",
					Name:      "ls",
					Arguments: []byte(`{"path":"."}`),
				}},
			}
		default:
			return measuredResponse(&llmwire.Response{
				ToolCalls: []llmwire.ToolCall{{
					ID:   taskCallID,
					Name: tool.IDTask,
					Arguments: []byte(
						`{"prompt":"CHILD_TASK do the thing","description":"child work",` +
							`"subagent_type":"general","background":true}`,
					),
				}},
			})
		}
	}

	h := newHarness(t, harnessOptions{respond: respond})
	defer h.shutdown()

	events := collectEvents(h.mgr.bus.SubscribeAll())
	defer events.stop()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "spawn then keep working", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)
	require.False(t, link.Blocking, "the scenario needs a background child")

	select {
	case <-compactionEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("automatic compaction never reached its summarization call")
	}

	// The child finishes while the parent is inside compaction: its completion is
	// produced against a transcript the parent is about to replace wholesale.
	close(childRelease)
	h.waitUntil("child terminalizes mid-compaction", func() bool {
		lnk, lerr := h.links.GetLink(h.ctx, link.ChildID)

		return lerr == nil && lnk != nil && lnk.Terminal()
	})

	close(compactionRelease)

	h.waitForDelivery(link.ChildID)
	// Delivery commits the inbox before the parent resumes; an idle gap is not completion.
	waitForIdleAfterMessage(t, events, parentID, "child completion handled")

	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs),
		"a completion committed around a compaction must not cross or orphan a tool pair")
	assert.Equal(t, 1, countSummaryRows(msgs), "exactly one summary row")
	assert.Equal(t, 1, countSubagentCompletions(msgs, link.ChildID), "exactly one completion record")
	assert.Zero(t, countToolResultsFor(msgs, "subagent_event"))

	// The launch pair was summarized away while the child was still out; the
	// completion is a self-contained event, so it still lands transcript-valid.
	assert.False(t, hasAssistantToolCall(msgs, tool.IDTask), "the launch pair was compacted mid-flight")
	assert.Zero(t, countToolResultsFor(msgs, tool.IDTask))

	// The completion is reachable by the model: it survives after the summary.
	summaryAt := indexOfSummary(msgs)
	require.GreaterOrEqual(t, summaryAt, 0)
	assert.Greater(t, indexOfSubagentCompletion(msgs), summaryAt,
		"the completion appended around compaction must not sort ahead of the summary")

	assert.Equal(t, "child completion handled", lastAssistantTextDTO(msgs),
		"the parent kept running after the completion landed")

	trace := events.snapshot()
	assert.Equal(t, 1, countPublishedMessage(trace, parentID, compactionStartNotice))
	assert.Equal(t, 1, countPublishedMessage(trace, parentID, compactionDoneNotice))
}

// The deferral notice announces a transition — "your /compact is now queued" —
// not a state. The daemon rebuilds the session (and its loopRunner) on every
// wake, so a parent woken repeatedly while its blocking child is out must not
// re-announce it once per wake.
func TestScenario_DeferredCompactAnnouncesItselfOncePerEpisode(t *testing.T) {
	release := make(chan struct{})

	h := newHarness(t, harnessOptions{respond: blockingCompactRespond(release)})
	collector := collectEvents(h.mgr.bus.SubscribeAll())

	closed := false

	defer func() {
		if !closed {
			close(release)
		}

		collector.stop()
		h.shutdown()
	}()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "do work then spawn", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)
	require.True(t, link.Blocking)

	h.waitUntil("parent suspended", func() bool { return !h.mgr.HasActiveLoop(parentID) })

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, parentID, "/compact"))

	collector.waitFor(
		t,
		"deferral notice reaches the controller",
		func(events []controllerapi.SessionNotification) bool {
			return countPublishedMessage(events, parentID, noticeCompactDeferred) == 1
		},
	)
	h.waitUntil("first deferred wake finished", func() bool { return !h.mgr.HasActiveLoop(parentID) })

	// Two more wakes while the same blocking child is still out. Each rebuilds
	// the session from durable state — the episode has not changed.
	for _, msg := range []string{"any progress?", "still there?"} {
		h.startInboxWake()
		require.NoError(t, h.mgr.sendToSession(h.ctx, parentID, msg))
		h.waitUntil("wake finished", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	}

	assert.Equal(t, 1, countPublishedMessage(collector.snapshot(), parentID, noticeCompactDeferred),
		"one deferral notice per deferral episode, however often the session is woken")

	close(release)
	closed = true

	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	// The episode ends by running: the deferred request is not silently dropped.
	collector.waitFor(t, "the deferred compaction runs", func(events []controllerapi.SessionNotification) bool {
		return countPublishedMessage(events, parentID, noticeCompacted) == 1
	})
	assert.True(t, hasSummaryRow(h.parentMessages(parentID)))
}

// Compaction's progress messages are only useful if they leave the session:
// pin the ordered trace a controller actually receives.
func TestScenario_CompactPublishesItsOrderedNoticeTrace(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: compactOnlyRespond})
	collector := collectEvents(h.mgr.bus.SubscribeAll())

	defer func() {
		collector.stop()
		h.shutdown()
	}()

	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "do some work", "fake-model", nil)
	require.NoError(t, err)
	h.mgr.waitIdle(sessionID)

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "/compact"))
	collector.waitFor(t, "first compaction reported", func(events []controllerapi.SessionNotification) bool {
		return countPublishedMessage(events, sessionID, noticeCompacted) == 1
	})
	h.mgr.waitIdle(sessionID)

	require.True(t, hasSummaryRow(h.parentMessages(sessionID)))

	// The confirmation turn the completion check added after the first
	// compact is a fresh raw group, so a second /compact summarizes it and
	// reports success again; the "Nothing to compact" branch is covered by
	// the loop-level compact command tests.
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "/compact"))
	collector.waitFor(t, "second compaction reported", func(events []controllerapi.SessionNotification) bool {
		return countPublishedMessage(events, sessionID, noticeCompacted) == 2
	})
	h.mgr.waitIdle(sessionID)

	assert.Equal(t,
		[]string{noticeCompacting, noticeCompacted, noticeCompacting, noticeCompacted},
		compactionNotices(collector.snapshot(), sessionID),
	)
}

// A subagent compacting its own context is housekeeping the human never asked
// for: the publish gate must keep those notices inside the tree.
func TestScenario_SubagentCompactionNoticesStayInsideTheTree(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	collector := collectEvents(h.mgr.bus.SubscribeAll())

	defer func() {
		collector.stop()
		h.shutdown()
	}()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "SPAWN_CHILD please", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)
	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	require.NoError(t, h.mgr.SendToChild(h.ctx, link.ChildID, "/compact"))
	h.waitUntil("child compacted", func() bool {
		stored, loadErr := h.store.LoadActiveMessages(h.ctx, link.ChildID)

		return loadErr == nil && hasSummaryRow(toDTO(stored))
	})
	h.mgr.waitIdle(link.ChildID)

	assert.Empty(t, compactionNotices(collector.snapshot(), link.ChildID),
		"a child's compaction notices must never reach a controller")
	assert.Empty(t, compactionNotices(collector.snapshot(), parentID),
		"nor may they be misattributed to the parent")
}

// The blocker this plan closes: /compact arriving while a blocking child is out
// must not compact the task tool_use away from under the child. The request
// waits in the durable inbox, and compaction runs only once the result is in.
func TestScenario_CompactWaitsForABlockingChildThenRuns(t *testing.T) {
	release := make(chan struct{})

	h := newHarness(t, harnessOptions{respond: blockingCompactRespond(release)})

	closed := false
	defer func() {
		if !closed {
			close(release)
		}

		h.shutdown()
	}()

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "do work then spawn", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)
	require.True(t, link.Blocking)

	h.waitUntil("parent suspended", func() bool { return !h.mgr.HasActiveLoop(parentID) })

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, parentID, "/compact"))

	// A non-sleep pending call keeps the session unrunnable, so nothing compacts:
	// the transcript still owes the task call its result.
	msgs := h.parentMessages(parentID)
	assert.False(t, hasSummaryRow(msgs), "compaction must not run while the call is out")
	assert.True(t, hasAssistantToolCall(msgs, "task"), "the tool_use the child owns is still there")
	assert.Zero(t, countToolResultsFor(msgs, "task"))

	close(release)
	closed = true

	h.waitForDelivery(link.ChildID)
	h.waitUntil("deferred compaction consumed child result", func() bool {
		messages := h.parentMessages(parentID)

		return hasSummaryRow(messages) && countToolResultsFor(messages, "task") == 1
	})
	h.mgr.waitIdle(parentID)

	// The result landed in its own tool_use first; only then did the queued
	// /compact run. Nothing is left dangling for ResolvePendingCall to fail on.
	final := h.parentMessages(parentID)
	assert.True(t, hasSummaryRow(final), "the deferred /compact ran once the call was settled")
	require.NoError(t, llm.ValidateToolPairing(final))
	// The verbatim tail is never empty (D3): the settled launch pair is the
	// newest group and stays verbatim instead of being summarized.
	assert.True(t, hasAssistantToolCall(final, "task"), "the settled pair stays verbatim in the tail")
	assert.Equal(t, 1, countToolResultsFor(final, "task"), "the result landed before the deferred compact ran")

	res, err := h.mgr.Result(h.ctx, link.ChildID)
	require.NoError(t, err)
	assert.Equal(t, subagent.StateCompleted, res.State, "no zombie link")
}

// The deferred request rides the durable inbox, so it survives the daemon dying
// between the /compact and the child's completion.
func TestScenario_DeferredCompactSurvivesADaemonRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "restart.db")
	release := make(chan struct{})

	first := newHarness(t, harnessOptions{dbPath: dbPath, respond: blockingCompactRespond(release)})

	first.startInboxWake()
	parentID, err := first.mgr.Send(first.ctx, first.projectID, "do work then spawn", "fake-model", nil)
	require.NoError(t, err)

	link := first.waitForChildLink(parentID)
	require.True(t, link.Blocking)

	first.waitUntil("parent suspended", func() bool { return !first.mgr.HasActiveLoop(parentID) })
	first.startInboxWake()
	require.NoError(t, first.mgr.sendToSession(first.ctx, parentID, "/compact"))

	require.False(t, hasSummaryRow(first.parentMessages(parentID)))

	// Daemon goes down with the child still running and /compact still queued.
	close(release)
	first.shutdown()

	// A second daemon on the same durable state: its sweep resumes the child,
	// whose completion revives the parent, which then finds the queued /compact.
	second := newHarness(t, harnessOptions{dbPath: dbPath, respond: blockingCompactRespond(nil)})
	defer second.shutdown()

	require.NoError(t, second.mgr.Start(second.ctx))

	second.waitUntil("deferred compaction ran after the restart", func() bool {
		return hasSummaryRow(second.parentMessages(parentID))
	})

	require.NoError(t, llm.ValidateToolPairing(second.parentMessages(parentID)))
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
		if isCompactionInstruction(msgs) {
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

	h := newHarness(t, harnessOptions{respond: respond})
	collector := collectEvents(h.mgr.bus.SubscribeAll())

	released := false

	defer func() {
		if !released {
			close(release)
		}

		collector.stop()
		h.shutdown()
	}()

	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "list the workdir", "fake-model", nil)
	require.NoError(t, err)

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the model was never asked for the first turn")
	}

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "/compact"))

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

// Explicit compact success remains the regression reference: a replaceable
// start becomes a persistent terminal result. The first turn settles a tool
// round so the /compact has two raw groups — the newest one stays verbatim.
func TestHarnessScenario_CompactSuccessChain(t *testing.T) {
	var calls int
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		calls++
		if calls == 1 {
			return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
				ID:        "ls-call-1",
				Name:      "ls",
				Arguments: []byte(`{"path":"."}`),
			}}}
		}

		if calls == 2 || calls == 3 {
			// The first stop is the hidden candidate; the confirmation turn
			// answers the same prompt again.
			return &llmwire.Response{Text: "First answer."}
		}

		// Later calls serve both the compaction summary and the post-compact
		// turn; the summary text lands in the brief either way.
		return &llmwire.Response{Text: "Compacted summary."}
	}

	h := newHarness(t, harnessOptions{respond: respond})
	defer h.shutdown()

	collector := collectEvents(h.mgr.bus.SubscribeAll())
	defer collector.stop()

	h.startInboxWake()
	root, err := h.mgr.Send(h.ctx, h.projectID, "first prompt", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)

	waitForVisibleMessage(t, collector, root, "First answer.")
	// Wait for the loop teardown so the /compact deterministically re-announces
	// the session instead of racing the runner cleanup.
	h.waitUntil("first runner gone", func() bool { return !h.mgr.HasActiveLoop(root) })

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root, "/compact keep the TODO state"))
	collector.waitFor(t, "compaction finished", func(events []controllerapi.SessionNotification) bool {
		return containsMessage(events, root, "✅ Context compacted")
	})

	controller := newChainController(t, h)
	drainScenarioClaims(t, "compact_success_chain.json", controller)
	collector.waitFor(t, "idle after compact", func(events []controllerapi.SessionNotification) bool {
		return containsState(events, root, sessionevent.StateIdle)
	})

	assertHarnessTrace(t, "compact_success_chain.json", collector.snapshot(), root)
}
