package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
)

// skillCompactRespond answers summarization prompts with a brief and every other
// turn with text, so each send reaches idle in one turn.
func skillCompactRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if len(msgs) == 1 && strings.Contains(msgs[0].Content, "HISTORY TO SUMMARIZE") {
		return &llmwire.Response{
			Text: "## Goal\nfollow the playbook\n## Progress\n- read it\n## Context for Continuation\ncarry on",
		}
	}

	return &llmwire.Response{Text: "work done"}
}

// A skill attached before a compaction is reattached after it — and the second
// compaction, which finds its own reattachment in the transcript, must neither
// duplicate it nor drop it.
func TestHarnessScenario_SkillSurvivesTwoCompactionsExactlyOnce(t *testing.T) {
	const skillName = "playbook"

	h, rec := newSkillHarness(t, map[string]string{
		skillName: skillDoc(skillName, "The playbook", "Follow these steps for $ARGUMENTS."),
	}, skillCompactRespond)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())

	defer func() {
		collector.stop()
		h.shutdown()
	}()

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "start the work", "fake-model", nil)
	require.NoError(t, err)
	h.mgr.waitIdle(sessionID)

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "/skill "+skillName+" the release"))
	h.waitUntil("skill attached", func() bool {
		return countMessagesWithSkill(h.parentMessages(sessionID), skillName) == 1
	})
	h.mgr.waitIdle(sessionID)

	compactOnce := func(round int) {
		require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "/compact"))
		collector.waitFor(t, "compaction reported", func(e []controllerapi.SessionNotification) bool {
			return countPublishedMessage(e, sessionID, noticeCompacted) == round
		})
		h.mgr.waitIdle(sessionID)
	}

	compactOnce(1)

	first := h.parentMessages(sessionID)
	require.True(t, hasSummaryRow(first))
	require.Equal(t, 1, countMessagesWithSkill(first, skillName), "the skill is reattached exactly once")

	// New work after the reattachment, so the second compaction has something to
	// summarize and must decide what to do with the envelope it wrote itself.
	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "keep going"))
	h.mgr.waitIdle(sessionID)

	compactOnce(2)

	second := h.parentMessages(sessionID)
	require.True(t, hasSummaryRow(second))
	assert.Equal(t, 1, countMessagesWithSkill(second, skillName),
		"a second compaction neither duplicates nor drops the reattached skill")

	var envelopes int

	for _, m := range second {
		if strings.HasPrefix(m.Content, "<skill>\n<name>"+skillName+"</name>") {
			envelopes++
		}
	}

	assert.Equal(t, 1, envelopes, "the survivor is a standalone envelope, not text quoted into the summary")

	// The reattachment is not decoration: the model gets it on the next turn.
	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "what next?"))
	h.mgr.waitIdle(sessionID)

	calls := rec.snapshot()
	require.NotEmpty(t, calls)
	assert.True(t, hasUserContaining(calls[len(calls)-1].msgs, "Follow these steps for the release."),
		"the compacted transcript still teaches the model the skill")
}
