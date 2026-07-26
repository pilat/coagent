package daemon

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/tool"
)

// autoCompactionPromptTokens is what the scripted provider reports as its own
// measurement. It sits above 0.85 of the harness client's 200k window, so the
// automatic threshold fires on a transcript still small enough to summarize.
const autoCompactionPromptTokens = 190000

const autoCompactionBrief = "## Goal\nfinish the scripted task\n" +
	"## Progress\n- ran one tool\n" +
	"## Context for Continuation\nkeep going"

const compactionStartNotice = "🔄 Compacting context..."

const compactionDoneNotice = "✅ Context compacted"

// measuredResponse is a scripted turn that reports a provider measurement over
// the compaction threshold — the only thing that arms the automatic path.
func measuredResponse(resp *llmwire.Response) *llmwire.Response {
	resp.Usage = &llmwire.MessageUsage{PromptTokens: autoCompactionPromptTokens}

	return resp
}

// isCompactionPrompt recognises the summarization call: one user message
// carrying the whole rendered conversation.
func isCompactionPrompt(msgs []llmwire.Message) bool {
	return len(msgs) == 1 && strings.Contains(msgs[0].Content, "Conversation:")
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

// TestScenario_AutomaticCompactionRunsInsideTheDaemon drives the threshold path
// (not /compact) end to end on a real daemon: the provider reports usage over the
// cutoff, the sanctioned compaction point fires, and the session keeps running on
// the rebuilt transcript.
func TestScenario_AutomaticCompactionRunsInsideTheDaemon(t *testing.T) {
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		switch {
		case isCompactionPrompt(msgs):
			return &llmwire.Response{Text: autoCompactionBrief}
		case hasUserContaining(msgs, contextSummaryPrefix):
			return &llmwire.Response{Text: "work complete"}
		case hasToolResultFor(msgs, "ls"):
			return &llmwire.Response{Text: "uncompacted fallback"}
		default:
			return measuredResponse(&llmwire.Response{ToolCalls: []llmwire.ToolCall{{
				ID:        "ls-call-1",
				Name:      "ls",
				Arguments: []byte(`{"path":"."}`),
			}}})
		}
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()

	events := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer events.stop()

	parentID, err := h.mgr.Send(h.ctx, h.projectID, "start the scripted work", "fake-model", nil)
	require.NoError(t, err)

	h.waitUntil("session finishes on the compacted transcript", func() bool {
		return !h.mgr.HasActiveLoop(parentID) &&
			lastAssistantTextDTO(h.parentMessages(parentID)) == "work complete"
	})

	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs), "the rebuilt transcript must stay provider-valid")

	// header → summary turn → (no reattachments here, the work dir has no skills).
	summaryAt := indexOfSummary(msgs)
	require.Positive(t, summaryAt, "the header survives ahead of the summary")
	require.Equal(t, 1, countSummaryRows(msgs), "exactly one summary row")
	assert.Equal(t, llmwire.RoleUser, msgs[0].Role)
	assert.Contains(t, msgs[0].Content, "start the scripted work")
	assert.Contains(t, msgs[summaryAt].Content, autoCompactionBrief)
	require.Greater(t, len(msgs), summaryAt+2)
	assert.Equal(t, registry.PostCompactionAssistantAck, msgs[summaryAt+1].Content)
	assert.True(t, strings.HasPrefix(msgs[summaryAt+2].Content, "[Post-compaction"))

	// Everything the summary replaced is gone, including the settled tool pair.
	assert.False(t, hasAssistantToolCall(msgs, "ls"), "the summarized tool_use is gone")
	assert.Zero(t, countToolResultsFor(msgs, "ls"))

	trace := events.snapshot()
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
		case hasToolResultFor(msgs, "subagent_event"):
			return &llmwire.Response{Text: "child completion handled"}
		case hasUserContaining(msgs, contextSummaryPrefix):
			return &llmwire.Response{Text: "parent continued after compaction"}
		case hasToolResultFor(msgs, tool.IDTask):
			return measuredResponse(&llmwire.Response{ToolCalls: []llmwire.ToolCall{{
				ID:        "ls-call-1",
				Name:      "ls",
				Arguments: []byte(`{"path":"."}`),
			}}})
		default:
			return measuredResponse(&llmwire.Response{ToolCalls: []llmwire.ToolCall{{
				ID:   taskCallID,
				Name: tool.IDTask,
				Arguments: []byte(
					`{"prompt":"CHILD_TASK do the thing","description":"child work",` +
						`"subagent_type":"general","background":true}`,
				),
			}}})
		}
	}

	h := newSubagentHarnessWith(t, respond)
	defer h.shutdown()

	events := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer events.stop()

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
	h.mgr.waitIdle(parentID)

	msgs := h.parentMessages(parentID)
	require.NoError(t, llm.ValidateToolPairing(msgs),
		"a completion committed around a compaction must not cross or orphan a tool pair")
	assert.Equal(t, 1, countSummaryRows(msgs), "exactly one summary row")
	assert.Equal(t, 1, countSubagentEvents(msgs, link.ChildID), "exactly one completion record")
	assert.Equal(t, 1, countToolResultsFor(msgs, "subagent_event"))

	// The launch pair was summarized away while the child was still out; the
	// completion is a self-contained event, so it still lands transcript-valid.
	assert.False(t, hasAssistantToolCall(msgs, tool.IDTask), "the launch pair was compacted mid-flight")
	assert.Zero(t, countToolResultsFor(msgs, tool.IDTask))

	// The completion is reachable by the model: it survives after the summary.
	summaryAt := indexOfSummary(msgs)
	require.GreaterOrEqual(t, summaryAt, 0)
	assert.Greater(t, indexOfSubagentEvent(msgs), summaryAt,
		"the completion appended around compaction must not sort ahead of the summary")

	assert.Equal(t, "child completion handled", lastAssistantTextDTO(msgs),
		"the parent kept running after the completion landed")

	trace := events.snapshot()
	assert.Equal(t, 1, countPublishedMessage(trace, parentID, compactionStartNotice))
	assert.Equal(t, 1, countPublishedMessage(trace, parentID, compactionDoneNotice))
}

func indexOfSubagentEvent(msgs []llmwire.Message) int {
	for i, m := range msgs {
		if m.Role == llmwire.RoleTool && m.ToolName == "subagent_event" {
			return i
		}
	}

	return -1
}
