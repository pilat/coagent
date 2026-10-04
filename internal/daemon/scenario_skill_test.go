package daemon

import (
	"encoding/json"
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
			return textReply("skill child done")
		}
		if hasToolResultFor(messages, tool.IDTask) {
			return textReply("skill task delivered")
		}
		return callReply(
			taskCallID, tool.IDTask,
			`{"skill":" REVIEW ","skill_args":"the diff","description":"review","subagent_type":"general"}`,
		)
	}
	h := newHarness(t, harnessOptions{respond: respond})
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
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(parentID) })
	select {
	case input := <-childInput:
		assert.Contains(t, input, "<skill>\n<name>review</name>")
		assert.Contains(t, input, "Review the diff.\n</skill>")
		assert.True(t, strings.HasPrefix(input, "[+0s "), input)
	case <-time.After(5 * time.Second):
		t.Fatal("skill-seeded child input was not observed")
	}
}

// One persistent skill receipt follows accepted input and closes the progress chain before later card reuse.
func TestHarnessScenario_SkillActivationReceiptOrdersTheOutputChain(t *testing.T) {
	modelFollowUpQueued := make(chan struct{})
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(msgs, "skill") {
			return textReply("model activation complete")
		}
		if hasUserContaining(msgs, "invoke the skill yourself") {
			return callReply("receipt-skill", "skill", `{"name":"review"}`)
		}
		if skillScenarioHasEnvelope(msgs) || hasToolResultFor(msgs, "ls") {
			if skillScenarioHasEnvelope(msgs) {
				<-modelFollowUpQueued
			}
			return textReply("probe answer")
		}
		return &llmwire.Response{
			Text:      "Running the first probe",
			ToolCalls: []llmwire.ToolCall{{ID: "receipt-ls", Name: "ls", Arguments: []byte(`{"path":"."}`)}},
		}
	}
	h := newHarness(t, harnessOptions{respond: respond})
	skillsDir := filepath.Join(h.workDir(), ".claude", "skills", "review")
	require.NoError(t, os.MkdirAll(skillsDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(skillsDir, "SKILL.md"), []byte(skillScenarioSkill), 0o600))
	defer h.shutdown()
	defer closeOnce(modelFollowUpQueued)

	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	root, err := h.mgr.Send(
		h.ctx, h.projectID, "start the skill scenario", "fake-model", managerAttrs(scenarioManagerID),
	)
	require.NoError(t, err)
	collector.waitMessage(root, "probe answer")
	// Settle before the next input: sending into a live loop keeps the same
	// runner and races the session re-creation the golden trace records.
	h.waitUntil("first turn settled", func() bool { return !h.mgr.HasActiveLoop(root) })

	// Explicit /skill activation through the durable input boundary.
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root, "/skill review"))
	collector.waitMessage(root, "🔧 Activated skill: review")

	// A skill tool commits its result within activation, so its next stop answers directly without an envelope follow-up.
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root, "invoke the skill yourself"))
	closeOnce(modelFollowUpQueued)
	collector.waitMessage(root, "model activation complete")
	controller := newChainController(t, h)
	drainScenarioClaims(t, "skill_activation_receipt.json", controller)
	collector.waitIdleAfter(root, "model activation complete")
	assertHarnessTrace(t, "skill_activation_receipt.json", collector.snapshot(), root)

	// Exactly one receipt row per activation path, each a persistent message
	// carrying the canonical skill name and nothing else.
	var receipts []outboxRow
	for _, row := range h.outbox(root) {
		if strings.HasPrefix(strings.ToLower(row.Content), "🔧 activated skill: ") {
			receipts = append(receipts, row)
		}
	}
	require.Len(t, receipts, 2, "one receipt per activation path")
	for _, row := range receipts {
		assert.Equal(t, "message_persistent", row.Type)
		assert.Equal(t, "🔧 Activated skill: review", row.Content)
	}

	// A replaceable progress output exists before the first receipt and later
	// progress starts after it: the persistent receipt closed the chain.
	var progressBefore int
	for _, row := range h.outbox(root) {
		if row.Type == "message_replaceable" && row.ID < receipts[0].ID {
			progressBefore++
		}
	}
	assert.Positive(t, progressBefore, "a replaceable card existed before the receipt")

	// The accepted input binds one skill receipt across promotion replay, published as its own persistent claim.
	var receiptClaim int
	for _, claim := range traceClaims(t, "skill_activation_receipt.json") {
		if claim.SourceKey == fmt.Sprintf("input:%d:skill_receipt", inputIDOf(t, h, root)) {
			receiptClaim++
		}
	}
	assert.Equal(t, 1, receiptClaim, "the receipt is claimed exactly once by its input key")

	// Explicit activation emits exactly one message receipt; model-tool receipts use their claim, and skill content stays
	// hidden.
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

// Compaction must reattach a skill once, including repeated compaction that encounters its existing reattachment.
func TestHarnessScenario_SkillSurvivesTwoCompactionsExactlyOnce(t *testing.T) {
	const skillName = "playbook"
	wrapped := newSkillHarness(t, map[string]string{
		skillName: skillDoc(skillName, "The playbook", "Follow these steps for $ARGUMENTS."),
	}, skillCompactRespond)
	h, rec := wrapped.harness, wrapped.recorder
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "start the work", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(sessionID) })
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "/skill "+skillName+" the release"))
	h.waitUntil("skill attached", func() bool {
		return countMessagesWithSkill(h.messages(sessionID), skillName) == 1
	})
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(sessionID) })
	compactOnce := func(round int) {
		h.startInboxWake()
		require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "/compact"))
		collector.waitFor(t, "compaction reported", func(e []controllerapi.SessionNotification) bool {
			return countPublishedMessage(e, sessionID, noticeCompacted) == round
		})
		h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(sessionID) })
	}
	compactOnce(1)
	first := h.messages(sessionID)
	require.True(t, hasSummaryRow(first))
	require.Equal(t, 1, countMessagesWithSkill(first, skillName), "the skill is reattached exactly once")

	// New work after the reattachment, so the second compaction has something to
	// summarize and must decide what to do with the envelope it wrote itself.
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "keep going"))
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(sessionID) })
	compactOnce(2)
	second := h.messages(sessionID)
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
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(sessionID) })
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
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "warm up", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(sessionID) })
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "/skill "+skillName+" v1.2.3"))
	h.waitUntil("skill expanded", func() bool {
		return countMessagesWithSkill(h.messages(sessionID), skillName) == 1
	})
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(sessionID) })
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

// Model discovery and direct user invocation are independent switches, and each one must gate only its own path.
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
			collector := collectEvents(t, h.mgr.bus.SubscribeAll())
			defer collector.stop()
			h.startInboxWake()
			sessionID, err := h.mgr.Send(h.ctx, h.projectID, "warm up", "fake-model", nil)
			require.NoError(t, err)
			h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(sessionID) })
			calls := rec.snapshot()
			require.NotEmpty(t, calls)
			assert.Equal(t, tc.wantInPrompt, strings.Contains(calls[0].system, "**"+tc.skill+"**"),
				"model-invocable skills — and only those — are announced in the system prompt")
			h.startInboxWake()
			require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "/skill "+tc.skill))
			if tc.wantExpanded {
				h.waitUntil("skill expanded", func() bool {
					return countMessagesWithSkill(h.messages(sessionID), tc.skill) == 1
				})
				h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(sessionID) })
				assert.Empty(t, warningNotices(collector.snapshot(), sessionID))
				return
			}
			collector.waitFor(t, "rejection notice", func(e []controllerapi.SessionNotification) bool {
				return len(warningNotices(e, sessionID)) > 0
			})
			h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(sessionID) })
			assert.Contains(t, warningNotices(collector.snapshot(), sessionID)[0], "skill unavailable: "+tc.skill)
			assert.Zero(t, countMessagesWithSkill(h.messages(sessionID), tc.skill))
			h.requireInboxDrained(sessionID)
		})
	}
}

// skillScenarioSkill is the project-local user-invocable skill the scenario
// activates through both the /skill command and the model skill tool.
const skillScenarioSkill = `---
name: review
description: Review the current change
---
Review the change carefully.
`

func skillScenarioHasEnvelope(msgs []llmwire.Message) bool {
	for _, m := range msgs {
		if m.Role == llmwire.RoleUser && strings.Contains(m.Content, "<name>review</name>") {
			return true
		}
	}
	return false
}

func traceClaims(t *testing.T, name string) []harnessTraceClaim {
	t.Helper()
	data, err := os.ReadFile(harnessTracePath(name))
	require.NoError(t, err)
	var file harnessTraceFile
	require.NoError(t, json.Unmarshal(data, &file))
	return file.Claims
}

func inputIDOf(t *testing.T, h *harness, root int64) int64 {
	t.Helper()
	var inputID int64
	require.NoError(t, h.db.QueryRow(`SELECT id FROM session_inbox
		WHERE session_id = ? AND raw_content = '/skill review'`, root).Scan(&inputID))
	return inputID
}

// skillCompactRespond answers summarization prompts with a brief and every other
// turn with text, so each send reaches idle in one turn.
func skillCompactRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if isCompactionInstruction(msgs) {
		return textReply("## Goal\nfollow the playbook\n## Progress\n- read it\n## Context for Continuation\ncarry on")
	}
	return textReply("work done")
}

type skillHarness struct {
	*harness
	recorder *skillRecorder
}

func newSkillHarness(
	t *testing.T,
	skills map[string]string,
	respond func(string, []llmwire.Message) *llmwire.Response,
) *skillHarness {
	t.Helper()
	rec := &skillRecorder{}
	h := newHarness(t, harnessOptions{respond: rec.wrap(respond)})
	for name, body := range skills {
		dir := filepath.Join(h.workDir(), ".claude", "skills", name)
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o600))
	}
	return &skillHarness{harness: h, recorder: rec}
}

func skillDoc(name, description, body string, extraFrontmatter ...string) string {
	var b strings.Builder
	b.WriteString("---\nname: " + name + "\ndescription: " + description + "\n")
	for _, line := range extraFrontmatter {
		b.WriteString(line + "\n")
	}
	b.WriteString("---\n" + body + "\n")
	return b.String()
}

// countMessagesWithSkill counts transcript rows carrying the named skill envelope.
func countMessagesWithSkill(msgs []llmwire.Message, name string) int {
	count := 0
	for _, m := range msgs {
		count += strings.Count(m.Content, "<name>"+name+"</name>")
	}
	return count
}

// warningNotices returns every ⚠️ line a controller saw for the session.
func warningNotices(events []controllerapi.SessionNotification, sessionID int64) []string {
	var out []string
	for _, event := range events {
		if event.SessionID != sessionID || event.Notification.Type != sessionevent.NotifyMessage {
			continue
		}
		if strings.HasPrefix(event.Notification.Message, "⚠️ ") {
			out = append(out, event.Notification.Message)
		}
	}
	return out
}

// An unknown skill is consumed and rejected once on the control plane, without any model call.
func TestHarnessScenario_UnknownSkillCommandIsRejectedOnceAndDrains(t *testing.T) {
	wrapped := newSkillHarness(t, nil, plainRespond)
	h, rec := wrapped.harness, wrapped.recorder
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "warm up", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(sessionID) })
	callsBefore := len(rec.snapshot())
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, sessionID, "/skill nonexistent"))
	collector.waitFor(t, "rejection notice reaches the controller", func(e []controllerapi.SessionNotification) bool {
		return len(warningNotices(e, sessionID)) > 0
	})
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(sessionID) })
	notices := warningNotices(collector.snapshot(), sessionID)
	require.Len(t, notices, 1, "exactly one rejection notice")
	assert.Contains(t, notices[0], "skill unavailable: nonexistent")
	h.requireInboxDrained(sessionID)
	msgs := h.messages(sessionID)
	assert.False(t, hasUserContaining(msgs, "/skill nonexistent"), "a rejected command never enters the transcript")
	assert.Len(t, rec.snapshot(), callsBefore, "a rejected command must not cost a model turn")
	assert.False(t, h.mgr.HasActiveLoop(sessionID))
}

// An empty session's rejected skill must settle activation instead of asking the provider to answer an empty
// conversation.
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
			collector := collectEvents(t, h.mgr.bus.SubscribeAll())
			defer collector.stop()
			if tc.claudeMD != "" {
				require.NoError(t, os.WriteFile(filepath.Join(h.workDir(), "CLAUDE.md"), []byte(tc.claudeMD), 0o600))
			}
			h.startInboxWake()
			sessionID, err := h.mgr.Send(h.ctx, h.projectID, "/skill nonexistent", "fake-model", nil)
			require.NoError(t, err)
			collector.waitFor(t, "rejection notice", func(e []controllerapi.SessionNotification) bool {
				return len(warningNotices(e, sessionID)) > 0
			})
			h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(sessionID) })
			assert.Len(t, warningNotices(collector.snapshot(), sessionID), 1, "exactly one rejection notice")
			h.requireInboxDrained(sessionID)
			assert.Empty(t, rec.snapshot(), "a conversation that asks nothing is never sent to the provider")
			assert.Len(t, h.messages(sessionID), tc.wantStart, "a rejected command writes nothing")
		})
	}
}
