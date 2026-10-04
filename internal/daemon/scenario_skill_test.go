package daemon

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/tool"
)

func TestHarnessScenario_SkillSeededTaskStartsWithRenderedEnvelope(t *testing.T) {
	childInput := make(chan string, 1)
	respond := func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "<name>review</name>") {
			for _, message := range messages {
				if message.Role == llmwire.RoleUser && strings.Contains(message.Content, "<name>review</name>") {
					select {
					case childInput <- message.Content:
					default:
					}
					break
				}
			}

			return &llmwire.Response{Text: "skill child done"}
		}
		if hasToolResultFor(messages, tool.IDTask) {
			return &llmwire.Response{Text: "skill task delivered"}
		}

		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID: taskCallID, Name: tool.IDTask,
			Arguments: []byte(
				`{"skill":" REVIEW ","skill_args":"the diff","description":"review","subagent_type":"general"}`,
			),
		}}}
	}

	h := newHarness(t, harnessOptions{respond: respond})
	defer h.shutdown()
	workDir, err := h.mgr.store.GetProjectWorkDir(h.ctx, h.projectID)
	require.NoError(t, err)
	skillDir := filepath.Join(workDir, ".claude", "skills", "review")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(`---
name: review
description: Review changes
---
Review $ARGUMENTS.
`), 0o600))

	h.startInboxWake()
	parentID, err := h.mgr.Send(h.ctx, h.projectID, "run review skill", "fake-model", nil)
	require.NoError(t, err)
	h.mgr.waitIdle(parentID)

	select {
	case input := <-childInput:
		assert.Contains(t, input, "<skill>\n<name>review</name>")
		assert.Contains(t, input, "Review the diff.\n</skill>")
		assert.True(t, strings.HasPrefix(input, "[+0s "), input)
	case <-time.After(5 * time.Second):
		t.Fatal("skill-seeded child input was not observed")
	}
}

// The user-visible skill-activation receipt closes the replaceable progress
// chain: one persistent receipt per activation, ordered after the accepted
// input and before any later progress reuses the pre-activation card.
func TestHarnessScenario_SkillActivationReceiptOrdersTheOutputChain(t *testing.T) {
	modelFollowUpQueued := make(chan struct{})
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(msgs, "skill") {
			return &llmwire.Response{Text: "model activation complete"}
		}

		if hasUserContaining(msgs, "invoke the skill yourself") {
			return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
				ID: "receipt-skill", Name: "skill", Arguments: []byte(`{"name":"review"}`),
			}}}
		}

		if skillScenarioHasEnvelope(msgs) || hasToolResultFor(msgs, "ls") {
			if skillScenarioHasEnvelope(msgs) {
				<-modelFollowUpQueued
			}

			return &llmwire.Response{Text: "probe answer"}
		}

		return &llmwire.Response{
			Text: "Running the first probe",
			ToolCalls: []llmwire.ToolCall{{
				ID: "receipt-ls", Name: "ls", Arguments: []byte(`{"path":"."}`),
			}},
		}
	}

	h := newHarness(t, harnessOptions{respond: respond})
	skillsDir := filepath.Join(h.workDir(), ".claude", "skills", "review")
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillsDir, "SKILL.md"), []byte(skillScenarioSkill), 0o600))
	defer h.shutdown()
	defer closeOnce(modelFollowUpQueued)

	collector := collectEvents(h.mgr.bus.SubscribeAll())
	defer collector.stop()

	h.startInboxWake()
	root, err := h.mgr.Send(h.ctx, h.projectID, "start the skill scenario", "fake-model", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	waitForVisibleMessage(t, collector, root, "probe answer")
	// Settle before the next input: sending into a live loop keeps the same
	// runner and races the session re-creation the golden trace records.
	h.waitUntil("first turn settled", func() bool { return !h.mgr.HasActiveLoop(root) })

	// Explicit /skill activation through the durable input boundary.
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root, "/skill review"))
	waitForVisibleMessage(t, collector, root, "🔧 Activated skill: review")

	// Model-initiated activation through the skill tool. The skill tool
	// commits its result in-activation, so the model's next stop answers the
	// activation directly; the envelope follow-up branch of the responder
	// never runs under the two-phase check.
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root, "invoke the skill yourself"))
	closeOnce(modelFollowUpQueued)
	waitForVisibleMessage(t, collector, root, "model activation complete")

	controller := newChainController(t, h)
	drainScenarioClaims(t, "skill_activation_receipt.json", controller)
	waitForIdleAfterMessage(t, collector, root, "model activation complete")

	assertHarnessTrace(t, "skill_activation_receipt.json", collector.snapshot(), root)

	// Exactly one receipt row per activation path, each a persistent message
	// carrying the canonical skill name and nothing else.
	var receipts []struct {
		ID      int64
		Type    string
		Content string
	}
	rows, err := h.db.Query(`SELECT id, type, content FROM session_outbox
		WHERE session_id = ? AND content LIKE '🔧 Activated skill: %' ORDER BY id`, root)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var row struct {
			ID      int64
			Type    string
			Content string
		}
		require.NoError(t, rows.Scan(&row.ID, &row.Type, &row.Content))
		receipts = append(receipts, row)
	}
	require.NoError(t, rows.Err())
	require.Len(t, receipts, 2, "one receipt per activation path")
	for _, row := range receipts {
		assert.Equal(t, "message_persistent", row.Type)
		assert.Equal(t, "🔧 Activated skill: review", row.Content)
	}

	// A replaceable progress output exists before the first receipt and later
	// progress starts after it: the persistent receipt closed the chain.
	var progressBefore int
	err = h.db.QueryRow(`SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND type = 'message_replaceable' AND id < ?`,
		root, receipts[0].ID).Scan(&progressBefore)
	require.NoError(t, err)
	assert.Positive(t, progressBefore, "a replaceable card existed before the receipt")

	// The receipt's source key binds it to its accepted input, so a promotion
	// replay cannot mint a second receipt, and the recorded claim chain proves
	// the receipt was published as its own persistent message.
	var receiptClaim int
	for _, claim := range traceClaims(t, "skill_activation_receipt.json") {
		if claim.SourceKey == fmt.Sprintf("input:%d:skill_receipt", inputIDOf(t, h, root)) {
			receiptClaim++
		}
	}
	assert.Equal(t, 1, receiptClaim, "the receipt is claimed exactly once by its input key")

	// The explicit /skill activation receipt reached the controller sink as a
	// message event exactly once (the model-tool receipt rides the direct
	// output of its tool claim, not a second standalone event), and no skill
	// content reaches the manager.
	trace := collector.snapshot()
	assert.Equal(t, 1, countPublishedMessage(trace, root, "🔧 Activated skill: review"))
	for _, event := range trace {
		if event.SessionID != root || event.Notification.Type != sessionevent.NotifyMessage {
			continue
		}

		assert.NotContains(t, event.Notification.Message, "Review the change carefully.",
			"no skill content reaches the manager")
	}
}

// A skill attached before a compaction is reattached after it — and the second
// compaction, which finds its own reattachment in the transcript, must neither
// duplicate it nor drop it.
func TestHarnessScenario_SkillSurvivesTwoCompactionsExactlyOnce(t *testing.T) {
	const skillName = "playbook"

	wrapped := newSkillHarness(t, map[string]string{
		skillName: skillDoc(skillName, "The playbook", "Follow these steps for $ARGUMENTS."),
	}, skillCompactRespond)
	h, rec := wrapped.harness, wrapped.recorder
	collector := collectEvents(h.mgr.bus.SubscribeAll())

	defer func() {
		collector.stop()
		h.shutdown()
	}()

	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "start the work", "fake-model", nil)
	require.NoError(t, err)
	h.mgr.waitIdle(sessionID)

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "/skill "+skillName+" the release"))
	h.waitUntil("skill attached", func() bool {
		return countMessagesWithSkill(h.parentMessages(sessionID), skillName) == 1
	})
	h.mgr.waitIdle(sessionID)

	compactOnce := func(round int) {
		h.startInboxWake()
		require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "/compact"))
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
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "keep going"))
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
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "what next?"))
	h.mgr.waitIdle(sessionID)

	calls := rec.snapshot()
	require.NotEmpty(t, calls)
	assert.True(t, hasUserContaining(calls[len(calls)-1].msgs, "Follow these steps for the release."),
		"the compacted transcript still teaches the model the skill")
}

// The expansion is programmatic: what reaches the provider is the rendered skill
// envelope with its arguments substituted, not the command the human typed.
func TestHarnessScenario_SkillCommandExpandsBeforeTheModelCall(t *testing.T) {
	const skillName = "release-notes"

	wrapped := newSkillHarness(t, map[string]string{
		skillName: skillDoc(skillName, "Draft release notes", "Draft notes for $ARGUMENTS."),
	}, plainRespond)
	h, rec := wrapped.harness, wrapped.recorder
	collector := collectEvents(h.mgr.bus.SubscribeAll())

	defer func() {
		collector.stop()
		h.shutdown()
	}()

	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "warm up", "fake-model", nil)
	require.NoError(t, err)
	h.mgr.waitIdle(sessionID)

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "/skill "+skillName+" v1.2.3"))
	h.waitUntil("expanded skill reaches the transcript", func() bool {
		return countMessagesWithSkill(h.parentMessages(sessionID), skillName) == 1
	})
	h.mgr.waitIdle(sessionID)

	calls := rec.snapshot()
	require.NotEmpty(t, calls)

	last := calls[len(calls)-1]
	require.True(t, hasUserContaining(last.msgs, "<skill>\n<name>"+skillName+"</name>"),
		"the model receives the rendered envelope")
	assert.True(t, hasUserContaining(last.msgs, "Draft notes for v1.2.3."), "arguments are substituted")
	assert.False(t, hasUserContaining(last.msgs, "/skill "+skillName), "the raw command never reaches the model")

	h.requireInboxDrained(sessionID)
	assert.Empty(t, warningNotices(collector.snapshot(), sessionID), "a successful invocation warns about nothing")
}

// Model discovery and direct user invocation are independent switches, and each
// one must gate only its own path.
func TestHarnessScenario_SkillInvocationPolicySeparatesUserAndModelPaths(t *testing.T) {
	for _, tc := range []struct {
		name         string
		skill        string
		frontmatter  string
		wantExpanded bool
		wantInPrompt bool
	}{
		{
			name:         "user-invocable false is refused for /skill but stays model-visible",
			skill:        "model-only",
			frontmatter:  "user-invocable: false",
			wantInPrompt: true,
		},
		{
			name:         "disable-model-invocation true hides the skill but keeps /skill working",
			skill:        "user-only",
			frontmatter:  "disable-model-invocation: true",
			wantExpanded: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := newSkillHarness(t, map[string]string{
				tc.skill: skillDoc(tc.skill, "policy probe", "Body of "+tc.skill+".", tc.frontmatter),
			}, plainRespond)
			h, rec := wrapped.harness, wrapped.recorder
			collector := collectEvents(h.mgr.bus.SubscribeAll())

			defer func() {
				collector.stop()
				h.shutdown()
			}()

			h.startInboxWake()
			sessionID, err := h.mgr.Send(h.ctx, h.projectID, "warm up", "fake-model", nil)
			require.NoError(t, err)
			h.mgr.waitIdle(sessionID)

			calls := rec.snapshot()
			require.NotEmpty(t, calls)
			assert.Equal(t, tc.wantInPrompt, strings.Contains(calls[0].system, "**"+tc.skill+"**"),
				"model-invocable skills — and only those — are announced in the system prompt")

			h.startInboxWake()
			require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "/skill "+tc.skill))

			if tc.wantExpanded {
				h.waitUntil("expanded skill reaches the transcript", func() bool {
					return countMessagesWithSkill(h.parentMessages(sessionID), tc.skill) == 1
				})
				h.mgr.waitIdle(sessionID)
				assert.Empty(t, warningNotices(collector.snapshot(), sessionID))

				return
			}

			collector.waitFor(t, "rejection notice", func(e []controllerapi.SessionNotification) bool {
				return len(warningNotices(e, sessionID)) > 0
			})
			h.mgr.waitIdle(sessionID)

			assert.Contains(t, warningNotices(collector.snapshot(), sessionID)[0], "skill unavailable: "+tc.skill)
			assert.Zero(t, countMessagesWithSkill(h.parentMessages(sessionID), tc.skill))
			h.requireInboxDrained(sessionID)
		})
	}
}

// A /skill for a skill that does not exist is resolved on the control plane: the
// durable row is consumed, the human is told once, and the model is never asked
// to answer a command that never became a message.
func TestHarnessScenario_UnknownSkillCommandIsRejectedOnceAndDrains(t *testing.T) {
	wrapped := newSkillHarness(t, nil, plainRespond)
	h, rec := wrapped.harness, wrapped.recorder
	collector := collectEvents(h.mgr.bus.SubscribeAll())

	defer func() {
		collector.stop()
		h.shutdown()
	}()

	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "warm up", "fake-model", nil)
	require.NoError(t, err)
	h.mgr.waitIdle(sessionID)

	callsBefore := len(rec.snapshot())

	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "/skill nonexistent"))
	collector.waitFor(t, "rejection notice reaches the controller", func(e []controllerapi.SessionNotification) bool {
		return len(warningNotices(e, sessionID)) > 0
	})
	h.mgr.waitIdle(sessionID)

	notices := warningNotices(collector.snapshot(), sessionID)
	require.Len(t, notices, 1, "exactly one rejection notice")
	assert.Contains(t, notices[0], "skill unavailable: nonexistent")

	h.requireInboxDrained(sessionID)

	msgs := h.parentMessages(sessionID)
	assert.False(t, hasUserContaining(msgs, "/skill nonexistent"), "a rejected command never enters the transcript")
	assert.Len(t, rec.snapshot(), callsBefore, "a rejected command must not cost a model turn")
	assert.False(t, h.mgr.HasActiveLoop(sessionID))
}

// The same rejection on a session with nothing in it yet. There is no settled
// assistant turn to end the activation, so a rejection that does not resolve the
// loop hands the provider a conversation that asks it nothing — an empty message
// list, or a project's AGENTS.md header on its own.
func TestHarnessScenario_UnknownSkillOnAFreshSessionCostsNoModelTurn(t *testing.T) {
	for _, tc := range []struct {
		name      string
		claudeMD  string
		wantStart int
	}{
		{name: "empty project"},
		{name: "project with a CLAUDE.md header", claudeMD: "# House rules\nBe careful.", wantStart: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wrapped := newSkillHarness(t, nil, plainRespond)
			h, rec := wrapped.harness, wrapped.recorder
			collector := collectEvents(h.mgr.bus.SubscribeAll())

			defer func() {
				collector.stop()
				h.shutdown()
			}()

			if tc.claudeMD != "" {
				require.NoError(t, os.WriteFile(
					filepath.Join(h.workDir(), "CLAUDE.md"), []byte(tc.claudeMD), 0o600,
				))
			}

			h.startInboxWake()
			sessionID, err := h.mgr.Send(h.ctx, h.projectID, "/skill nonexistent", "fake-model", nil)
			require.NoError(t, err)

			collector.waitFor(t, "rejection notice", func(e []controllerapi.SessionNotification) bool {
				return len(warningNotices(e, sessionID)) > 0
			})
			h.mgr.waitIdle(sessionID)

			assert.Len(t, warningNotices(collector.snapshot(), sessionID), 1, "exactly one rejection notice")
			h.requireInboxDrained(sessionID)
			assert.Empty(t, rec.snapshot(), "a conversation that asks nothing is never sent to the provider")
			assert.Len(t, h.parentMessages(sessionID), tc.wantStart, "a rejected command writes nothing")
		})
	}
}
