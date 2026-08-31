package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionevent"
)

// The controller-visible compaction vocabulary, duplicated from internal/session
// on purpose: these strings are the contract with the human, not an implementation
// detail, so a reword must break a test outside the package that emits them.
const (
	noticeCompacting       = "🔄 Compacting context..."
	noticeCompacted        = "✅ Context compacted"
	noticeCompactionFailed = "❌ Compaction failed"
	noticeNothingToCompact = "Nothing to compact"
	noticeCompactDeferred  = "⏳ Compaction deferred until the session finishes waiting"
)

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

// compactOnlyRespond answers summarization prompts with a brief, does one
// unmeasured tool round before settling (the verbatim tail is never empty, so
// a compactable transcript needs two raw groups), and answers everything else
// with plain text.
func compactOnlyRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if len(msgs) == 1 && strings.Contains(msgs[0].Content, "HISTORY TO SUMMARIZE") {
		return &llmwire.Response{
			Text: "## Goal\nsome work\n## Progress\n- done\n## Context for Continuation\ncarry on",
		}
	}

	if hasToolResultFor(msgs, "ls") {
		return &llmwire.Response{Text: "work done"}
	}

	return &llmwire.Response{
		ToolCalls: []llmwire.ToolCall{{
			ID:        "ls-1",
			Name:      "ls",
			Arguments: []byte(`{"path":"."}`),
		}},
	}
}

// The deferral notice announces a transition — "your /compact is now queued" —
// not a state. The daemon rebuilds the session (and its loopRunner) on every
// wake, so a parent woken repeatedly while its blocking child is out must not
// re-announce it once per wake.
func TestScenario_DeferredCompactAnnouncesItselfOncePerEpisode(t *testing.T) {
	release := make(chan struct{})

	h := newSubagentHarnessWith(t, blockingCompactRespond(release))
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())

	closed := false

	defer func() {
		if !closed {
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

	require.NoError(t, h.mgr.SendToSession(h.ctx, parentID, "/compact"))

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
		require.NoError(t, h.mgr.SendToSession(h.ctx, parentID, msg))
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
	h := newSubagentHarnessWith(t, compactOnlyRespond)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())

	defer func() {
		collector.stop()
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "do some work", "fake-model", nil)
	require.NoError(t, err)
	h.mgr.waitIdle(sessionID)

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "/compact"))
	collector.waitFor(t, "first compaction reported", func(events []controllerapi.SessionNotification) bool {
		return countPublishedMessage(events, sessionID, noticeCompacted) == 1
	})
	h.mgr.waitIdle(sessionID)

	require.True(t, hasSummaryRow(h.parentMessages(sessionID)))

	// A second /compact finds only what the first one wrote, so it has nothing
	// left to summarize — and must say so rather than silently doing nothing.
	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "/compact"))
	collector.waitFor(t, "second compaction reported", func(events []controllerapi.SessionNotification) bool {
		return countPublishedMessage(events, sessionID, noticeNothingToCompact) == 1
	})
	h.mgr.waitIdle(sessionID)

	assert.Equal(t,
		[]string{noticeCompacting, noticeCompacted, noticeCompacting, noticeNothingToCompact},
		compactionNotices(collector.snapshot(), sessionID),
	)
}

// A subagent compacting its own context is housekeeping the human never asked
// for: the publish gate must keep those notices inside the tree.
func TestScenario_SubagentCompactionNoticesStayInsideTheTree(t *testing.T) {
	h := newSubagentHarness(t)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())

	defer func() {
		collector.stop()
		h.shutdown()
	}()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "SPAWN_CHILD please", "fake-model", nil)
	require.NoError(t, err)

	link := h.waitForChildLink(parentID)
	h.waitForDelivery(link.ChildID)
	h.mgr.waitIdle(parentID)

	require.NoError(t, h.mgr.SendToChild(h.ctx, link.ChildID, "/compact"))
	h.waitUntil("child compacted", func() bool {
		stored, loadErr := h.sessStore.LoadActiveMessages(h.ctx, link.ChildID)

		return loadErr == nil && hasSummaryRow(toDTO(stored))
	})
	h.mgr.waitIdle(link.ChildID)

	assert.Empty(t, compactionNotices(collector.snapshot(), link.ChildID),
		"a child's compaction notices must never reach a controller")
	assert.Empty(t, compactionNotices(collector.snapshot(), parentID),
		"nor may they be misattributed to the parent")
}
