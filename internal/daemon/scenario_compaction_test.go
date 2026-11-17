package daemon

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm/llmtest"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

// Provider usage triggers real automatic compaction; two raw groups retain a tail while leaving history to summarize.
func TestScenario_AutomaticCompactionRunsInsideTheDaemon(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		switch {
		case isCompactionInstruction(msgs):
			return textReply(autoCompactionBrief)
		case hasUserContaining(msgs, contextSummaryPrefix):
			return textReply("work complete")
		case hasToolResultFor(msgs, "read"):
			return textReply("uncompacted fallback")
		case hasToolResultFor(msgs, "ls"):
			return callReply("read-followup", "read", `{"file_path":"go.mod"}`)
		default:
			return scriptedToolCall("ls", `{"path":"."}`)
		}
	}
	h := newHarness(t, harnessOptions{respond: respond})

	events := h.collect()
	parentID := h.send("start the scripted work", nil)
	h.waitUntil("compacted answer", func() bool {
		return !h.mgr.HasActiveLoop(parentID) &&
			lastAssistantTextDTO(h.messages(parentID)) == "work complete"
	})
	msgs := h.messages(parentID)
	require.NoError(t, llmtest.ValidateToolPairing(msgs), "the rebuilt transcript must stay provider-valid")

	// Compaction preserves the header, marked summary, verbatim follow-up tail and resumed work in order.
	summaryAt := indexOfSummary(msgs)
	require.Positive(t, summaryAt, "the header survives ahead of the summary")
	require.Equal(t, 1, countSummaryRows(msgs), "exactly one summary row")
	assert.Equal(t, llmwire.RoleUser, msgs[0].Role)
	assert.Contains(t, msgs[0].Content, "start the scripted work")
	assert.Contains(t, msgs[summaryAt].Content, autoCompactionBrief)
	require.Greater(t, len(msgs), summaryAt+2, "a verbatim tail survives behind the summary")
	assert.Equal(t, "work complete", msgs[len(msgs)-1].Content, "the loop continued on the checkpoint")

	// Everything the summary replaced is gone, including the settled tool pair.
	assert.False(t, (countAssistantToolCallsFor(msgs, "ls") > 0), "the summarized tool_use is gone")
	assert.Zero(t, countToolResultsFor(msgs, "ls"))
	trace := events.snapshot()
	for i, e := range trace {
		t.Logf("TRACE %d: sess=%d type=%s msg=%q", i, e.SessionID, e.Notification.Type, e.Notification.Message)
	}
	assert.Equal(t, 1, countPublishedMessage(trace, parentID, noticeCompacting))
	assert.Equal(t, 1, countPublishedMessage(trace, parentID, noticeCompacted))
	assert.Zero(t, countPublishedMessage(trace, parentID, "❌ Compaction failed"))
}

// Automatic compaction may run during a background child; its later completion still pairs and delivers exactly once.
func TestScenario_AutoCompactionWhileABackgroundChildIsInFlight(t *testing.T) {
	childRelease := make(chan struct{})
	compactionEntered := make(chan struct{})
	compactionRelease := make(chan struct{})
	var enteredOnce sync.Once
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		switch {
		case isCompactionInstruction(msgs):
			// Release the child during compaction so its completion races the parent's stale snapshot.
			enteredOnce.Do(func() { close(compactionEntered) })
			<-compactionRelease

			return textReply(autoCompactionBrief)
		case hasUserContaining(msgs, "CHILD_TASK"):
			<-childRelease

			return textReply("background child done: 7")
		case hasUserContaining(msgs, "<subagent_completion>"):
			return textReply("child completion handled")
		case hasUserContaining(msgs, contextSummaryPrefix):
			return textReply("parent continued after compaction")
		case hasToolResultFor(msgs, tool.IDTask):
			// The unmeasured follow-up preserves the newest raw group while compaction summarizes the in-flight launch pair.
			return callReply("ls-followup", "ls", `{"path":"."}`)
		default:
			return measuredResponse(
				callReply(taskCallID, tool.IDTask, `{"prompt":"CHILD_TASK do the thing","description":"child work",`+
					`"subagent_type":"general","background":true}`),
			)
		}
	}
	h := newHarness(t, harnessOptions{respond: respond})

	events := h.collect()
	parentID := h.send("spawn then keep working", nil)
	link := h.waitLink(parentID, taskCallID)
	require.False(t, link.Blocking, "the scenario needs a background child")
	select {
	case <-compactionEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("automatic compaction never reached its summarization call")
	}

	// The child completion must survive replacement of the parent transcript during compaction.
	close(childRelease)
	h.waitUntil("child completed", func() bool {
		lnk, lerr := h.links.GetLink(h.ctx, link.ChildID)
		return lerr == nil && lnk != nil && lnk.Terminal()
	})
	close(compactionRelease)
	h.waitDelivered(link.ChildID)
	// Delivery commits the inbox before the parent resumes; an idle gap is not completion.
	events.waitIdleAfter(parentID, "child completion handled")
	msgs := h.messages(parentID)
	require.NoError(t, llmtest.ValidateToolPairing(msgs),
		"a completion committed around a compaction must not cross or orphan a tool pair")
	assert.Equal(t, 1, countSummaryRows(msgs), "exactly one summary row")
	completionCount := countSubagentCompletions(msgs, link.ChildID)
	assert.Equal(t, 1, completionCount, "exactly one completion record")
	assert.Zero(t, countToolResultsFor(msgs, "subagent_event"))

	// A summarized launch pair must not invalidate its later self-contained child completion.
	assert.False(t, (countAssistantToolCallsFor(msgs, tool.IDTask) > 0), "the launch pair was compacted mid-flight")
	assert.Zero(t, countToolResultsFor(msgs, tool.IDTask))

	// The completion is reachable by the model: it survives after the summary.
	summaryAt := indexOfSummary(msgs)
	require.GreaterOrEqual(t, summaryAt, 0)
	assert.Greater(t, indexOfSubagentCompletion(msgs), summaryAt,
		"the completion appended around compaction must not sort ahead of the summary")
	assert.Equal(t, "child completion handled", lastAssistantTextDTO(msgs),
		"the parent kept running after the completion landed")
	trace := events.snapshot()
	assert.Equal(t, 1, countPublishedMessage(trace, parentID, noticeCompacting))
	assert.Equal(t, 1, countPublishedMessage(trace, parentID, noticeCompacted))
}

// A deferred compact notice marks a transition; repeated wakes must not re-announce it from rebuilt runners.
func TestScenario_DeferredCompactAnnouncesItselfOncePerEpisode(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, harnessOptions{respond: blockingCompactRespond(release)})
	collector := h.collect()
	defer closeOnce(release)
	parentID, link := parkOnBlockingChild(t, h)
	h.say(parentID, "/compact")
	collector.waitFor(
		t,
		"deferral notice reaches the controller",
		func(events []controllerapi.SessionNotification) bool {
			return countPublishedMessage(events, parentID, noticeCompactDeferred) == 1
		},
	)
	h.waitUntil("first deferred wake finished", func() bool { return !h.mgr.HasActiveLoop(parentID) })

	// Repeated wakes rebuild durable state without starting a new episode for the pending child.
	for _, msg := range []string{"any progress?", "still there?"} {
		h.say(parentID, msg)
		h.waitUntil("wake finished", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	}
	assert.Equal(t, 1, countPublishedMessage(collector.snapshot(), parentID, noticeCompactDeferred),
		"one deferral notice per deferral episode, however often the session is woken")
	close(release)
	h.waitDelivered(link.ChildID)
	h.waitAnswer(parentID, "")

	// The episode ends by running: the deferred request is not silently dropped.
	collector.waitFor(t, "the deferred compaction runs", func(events []controllerapi.SessionNotification) bool {
		return countPublishedMessage(events, parentID, noticeCompacted) == 1
	})
	assert.True(t, hasSummaryRow(h.messages(parentID)))
}

// Compaction progress must reach the controller in the recorded order.
func TestScenario_CompactPublishesItsOrderedNoticeTrace(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: compactOnlyRespond})
	collector := h.collect()
	sessionID := h.send("do some work", nil)
	h.waitAnswer(sessionID, "")
	h.say(sessionID, "/compact")
	collector.waitFor(t, "first compaction reported", func(events []controllerapi.SessionNotification) bool {
		return countPublishedMessage(events, sessionID, noticeCompacted) == 1
	})
	h.waitAnswer(sessionID, "")
	require.True(t, hasSummaryRow(h.messages(sessionID)))

	// Confirmation creates another raw group, allowing a second compact; the loop tests cover the empty-history branch.
	h.say(sessionID, "/compact")
	collector.waitFor(t, "second compaction reported", func(events []controllerapi.SessionNotification) bool {
		return countPublishedMessage(events, sessionID, noticeCompacted) == 2
	})
	h.waitAnswer(sessionID, "")
	assert.Equal(t,
		[]string{noticeCompacting, noticeCompacted, noticeCompacting, noticeCompacted},
		compactionNotices(collector.snapshot(), sessionID),
	)
}

func TestScenario_ParentSlashTextReachesTheChildAsText(t *testing.T) {
	h := newHarness(t, harnessOptions{})
	parentID := h.send("SPAWN_CHILD please", nil)
	link := h.waitLink(parentID, taskCallID)
	h.waitDelivered(link.ChildID)
	h.waitAnswer(parentID, "")
	before := len(h.messages(link.ChildID))
	const content = "/compact keep the todos"
	require.NoError(t, h.mgr.SendToChild(h.ctx, link.ChildID, "/compact keep the todos"))
	h.waitUntil("child answered the slash text", func() bool {
		return !h.mgr.HasActiveLoop(link.ChildID) && hasUserContaining(h.messages(link.ChildID), content)
	})
	msgs := h.messages(link.ChildID)
	assert.False(t, hasSummaryRow(msgs))
	require.GreaterOrEqual(t, len(msgs), before+2)
	inputAt := -1
	for i, msg := range msgs {
		if msg.Role == llmwire.RoleUser && strings.Contains(msg.Content, content) {
			inputAt = i
		}
	}
	require.GreaterOrEqual(t, inputAt, before)
	var reply string
	for _, msg := range msgs[inputAt+1:] {
		if msg.Role == llmwire.RoleAssistant && msg.Content != "" {
			reply = msg.Content
			break
		}
	}
	assert.Equal(t, "child finished: 42", reply)
}

// Child housekeeping notices must remain inside the tree instead of reaching the human.
func TestScenario_SubagentCompactionNoticesStayInsideTheTree(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		switch {
		case isCompactionInstruction(msgs):
			return textReply(autoCompactionBrief)
		case hasUserContaining(msgs, "CHILD_TASK"):
			switch {
			case hasUserContaining(msgs, contextSummaryPrefix), hasToolResultFor(msgs, "read"):
				return textReply("child finished: 42")
			case hasToolResultFor(msgs, "ls"):
				return callReply("read-followup", "read", `{"file_path":"go.mod"}`)
			default:
				return scriptedToolCall("ls", `{"path":"."}`)
			}
		case hasToolResultFor(msgs, tool.IDTask), hasUserContaining(msgs, "<subagent_completion>"):
			return textReply("all set, child launched")
		default:
			return callReply(taskCallID, tool.IDTask,
				`{"prompt":"CHILD_TASK do the thing","description":"child work",`+
					`"subagent_type":"general","background":true}`)
		}
	}
	h := newHarness(t, harnessOptions{respond: respond})
	collector := h.collect()
	parentID := h.send("SPAWN_CHILD please", nil)
	link := h.waitLink(parentID, taskCallID)
	h.waitDelivered(link.ChildID)
	h.waitAnswer(parentID, "")
	h.waitUntil("child compacted", func() bool {
		stored, loadErr := h.store.LoadActiveMessages(h.ctx, link.ChildID)
		return loadErr == nil && hasSummaryRow(toDTO(stored))
	})
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(link.ChildID) })
	assert.Empty(t, compactionNotices(collector.snapshot(), link.ChildID),
		"a child's compaction notices must never reach a controller")
	assert.Empty(t, compactionNotices(collector.snapshot(), parentID), "nor may they be misattributed to the parent")
}

// Compact must wait for the blocking child's result instead of deleting the tool call that the child still owns.
func TestScenario_CompactWaitsForABlockingChildThenRuns(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, harnessOptions{respond: blockingCompactRespond(release)})
	defer closeOnce(release)
	parentID, link := parkOnBlockingChild(t, h)
	h.say(parentID, "/compact")

	// A pending task result keeps the session unrunnable and prevents compaction.
	msgs := h.messages(parentID)
	assert.False(t, hasSummaryRow(msgs), "compaction must not run while the call is out")
	assert.True(t, (countAssistantToolCallsFor(msgs, "task") > 0), "the tool_use the child owns is still there")
	assert.Zero(t, countToolResultsFor(msgs, "task"))
	close(release)
	h.waitDelivered(link.ChildID)
	h.waitUntil("deferred compact answered", func() bool {
		messages := h.messages(parentID)
		return hasSummaryRow(messages) && countToolResultsFor(messages, "task") == 1
	})
	h.waitAnswer(parentID, "")

	// Settle the tool result before queued compaction so pending-call resolution sees no dangling pair.
	final := h.messages(parentID)
	assert.True(t, hasSummaryRow(final), "the deferred /compact ran once the call was settled")
	require.NoError(t, llmtest.ValidateToolPairing(final))
	// The newest settled launch pair must remain in the nonempty verbatim tail.
	assert.True(t, (countAssistantToolCallsFor(final, "task") > 0), "the settled pair stays verbatim in the tail")
	assert.Equal(t, 1, countToolResultsFor(final, "task"), "the result landed before the deferred compact ran")
	res, err := h.mgr.Result(h.ctx, link.ChildID)
	require.NoError(t, err)
	assert.Equal(t, subagent.StateCompleted, res.State, "no zombie link")
}

// Durable inbox delivery must preserve deferred compaction across the daemon crash.
func TestScenario_DeferredCompactSurvivesADaemonRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "restart.db")
	release := make(chan struct{})
	first := newHarness(t, harnessOptions{dbPath: dbPath, respond: blockingCompactRespond(release)})
	parentID, _ := parkOnBlockingChild(t, first)
	first.say(parentID, "/compact")
	require.False(t, hasSummaryRow(first.messages(parentID)))

	// Daemon goes down with the child still running and /compact still queued.
	close(release)
	first.shutdown()

	// Recovery resumes the child, whose completion revives the parent and its queued compaction.
	second := first.restart(blockingCompactRespond(nil))
	require.NoError(t, second.mgr.Start(second.ctx))
	second.waitUntil("restart compact completed", func() bool {
		return hasSummaryRow(second.messages(parentID))
	})
	require.NoError(t, llmtest.ValidateToolPairing(second.messages(parentID)))
}

// Compact cannot abandon interrupted work; a settled first round supplies history beyond the mandatory verbatim tail.
func TestHarnessScenario_CompactMidActivationStillAnswersTheInterruptedWork(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if isCompactionInstruction(msgs) {
			return textReply(
				"## Goal\nlist the workdir\n## Progress\n- listed\n## Context for Continuation\nreport back",
			)
		}
		if hasToolResultFor(msgs, "read") || hasSummaryRow(msgs) {
			return textReply("work done")
		}
		if hasToolResultFor(msgs, "ls") {
			// The second turn hangs until the /compact has landed mid-activation.
			once.Do(func() { close(entered) })
			<-release
			return callReply("read-call-1", "read", `{"file_path":"go.mod"}`)
		}
		return callReply("ls-call-1", "ls", `{"path":"."}`)
	}
	h := newHarness(t, harnessOptions{respond: respond})
	collector := h.collect()
	defer closeOnce(release)
	sessionID := h.send("list the workdir", nil)
	require.True(t, closedInTime(entered), "the model was never asked for the first turn")
	h.say(sessionID, "/compact")
	close(release)
	collector.waitFor(t, "the requested compaction runs", func(e []controllerapi.SessionNotification) bool {
		return countPublishedMessage(e, sessionID, noticeCompacted) == 1
	})
	h.waitAnswer(sessionID, "")
	collector.waitFor(t, "the interrupted work is still answered", func(e []controllerapi.SessionNotification) bool {
		return countPublishedMessage(e, sessionID, "work done") == 1
	})
	msgs := h.messages(sessionID)
	require.NoError(t, llmtest.ValidateToolPairing(msgs))
	assert.True(t, hasSummaryRow(msgs), "the requested compaction ran")
	h.requireInboxDrained(sessionID)
}

// Compact success replaces its start with a persistent result; the settled tool round keeps history beyond the tail.
func TestHarnessScenario_CompactSuccessChain(t *testing.T) {
	var calls int
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		calls++
		if calls == 1 {
			return callReply("ls-call-1", "ls", `{"path":"."}`)
		}
		if calls == 2 || calls == 3 {
			// The first stop is the hidden candidate; the confirmation turn answers the same prompt again.
			return textReply("First answer.")
		}

		// The summary text must reach the brief whether the call serves compaction or the resumed turn.
		return textReply("Compacted summary.")
	}
	h := newHarness(t, harnessOptions{respond: respond})

	collector := h.collect()
	root := h.send("first prompt", managerAttrs(scenarioManagerID))
	collector.waitMessage(root, "First answer.")
	// Wait for teardown so compaction deterministically re-announces the session.
	h.waitUntil("first runner gone", func() bool { return !h.mgr.HasActiveLoop(root) })
	h.say(root, "/compact keep the TODO state")
	collector.waitFor(t, "compaction finished", func(events []controllerapi.SessionNotification) bool {
		for _, event := range events {
			if event.SessionID == root && event.Notification.Message == "✅ Context compacted" {
				return true
			}
		}
		return false
	})
	collector.waitFor(t, "idle after compact", func(events []controllerapi.SessionNotification) bool {
		return hasStateEvent(events, root, sessionevent.StateIdle)
	})
	assertHarnessTrace(t, h, "compact_success_chain.json", collector.snapshot(), root)
}

// Usage exceeds the 85% threshold of the 200k window while leaving the transcript small enough to summarize.
const autoCompactionPromptTokens = 190000

const autoCompactionBrief = "## Goal\nfinish the scripted task\n" +
	"## Progress\n- ran one tool\n" +
	"## Context for Continuation\nkeep going"

// Only a provider usage measurement above the threshold may arm automatic compaction.
func measuredResponse(resp *llmwire.Response) *llmwire.Response {
	resp.Usage = &llmwire.MessageUsage{PromptTokens: autoCompactionPromptTokens}
	return resp
}

var (
	scriptedCallMu      sync.Mutex
	scriptedCallCounter int
)

// Unique call IDs match real provider behavior; reuse across assistant turns would corrupt pairing validity.
func scriptedToolCall(name, args string) *llmwire.Response {
	scriptedCallMu.Lock()
	defer scriptedCallMu.Unlock()
	scriptedCallCounter++
	return measuredResponse(callReply(fmt.Sprintf("%s-call-%d", name, scriptedCallCounter), name, args))
}

func indexOfSummary(msgs []llmwire.Message) int {
	for i, m := range msgs {
		if strings.HasPrefix(m.Content, contextSummaryPrefix) {
			return i
		}
	}
	return -1
}

func countSummaryRows(msgs []llmwire.Message) int {
	count := 0
	for _, m := range msgs {
		if strings.HasPrefix(m.Content, contextSummaryPrefix) {
			count++
		}
	}
	return count
}

func indexOfSubagentCompletion(msgs []llmwire.Message) int {
	for i, m := range msgs {
		if m.Role == llmwire.RoleUser && strings.Contains(m.Content, "<subagent_completion>") {
			return i
		}
	}
	return -1
}

func compactionNotices(events []controllerapi.SessionNotification, sessionID int64) []string {
	vocabulary := map[string]bool{
		noticeCompacting:       true,
		noticeCompacted:        true,
		noticeCompactionFailed: true,
		noticeNothingToCompact: true,
		noticeCompactDeferred:  true,
	}
	var out []string
	for _, event := range events {
		if event.SessionID != sessionID || event.Notification.Type != sessionevent.NotifyMessage {
			continue
		}
		if vocabulary[event.Notification.Message] {
			out = append(out, event.Notification.Message)
		}
	}
	return out
}

// An unmeasured tool round creates an older raw group to summarize while retaining the newest group verbatim.
func compactOnlyRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if isCompactionInstruction(msgs) {
		return textReply("## Goal\nsome work\n## Progress\n- done\n## Context for Continuation\ncarry on")
	}
	if hasToolResultFor(msgs, "ls") {
		return textReply("work done")
	}
	return callReply("ls-1", "ls", `{"path":"."}`)
}
