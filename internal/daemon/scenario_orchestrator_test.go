package daemon

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/orchestrator"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

func TestOrchestratorToolsOnlyOnModeRoots(t *testing.T) {
	h := newRegistryPromptHarness(t, func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "CHILD_TOOL_CHECK") {
			return textReply("child checked")
		}
		if hasUserContaining(messages, "start child") && !hasToolResultFor(messages, "task") {
			return callReply(
				"mode-child",
				"task",
				`{"prompt":"CHILD_TOOL_CHECK","description":"check tools","subagent_type":"general"}`,
			)
		}
		return textReply("done")
	})
	h.startInboxWake()
	root := h.send("initial", managerAttrs("owner"))
	h.waitAnswer(root, "done")
	assertModeToolSchemas(t, h.schemas.first(t, root), false)
	_, err := h.store.SetOrchestrator(h.ctx, root, true)
	require.NoError(t, err)
	h.say(root, "start child")
	h.waitUntil("child launched", func() bool { return h.linkByCall(root, "mode-child") != nil })
	child := h.linkByCall(root, "mode-child").ChildID
	h.waitAnswer(child, "child checked")
	h.waitAnswer(root, "done")
	assertModeToolSchemas(t, h.schemas.last(t, root), true)
	assertModeToolSchemas(t, h.schemas.first(t, child), false)
	ownerless := h.createRoot(nil)
	_, err = h.db.ExecContext(h.ctx, `UPDATE sessions SET attributes = '{"orchestrator":true}' WHERE id = ?`, ownerless)
	require.NoError(t, err)
	h.say(ownerless, "ownerless")
	h.waitAnswer(ownerless, "done")
	assertModeToolSchemas(t, h.schemas.first(t, ownerless), false)
}

func TestOrchestratorSendRefusals(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	h.startInboxWake()
	caller := modeRoot(t, h)
	foreign := h.createRoot(managerAttrs("other"))
	child := h.createChild(caller, subagent.Link{TaskCallID: "owned-child"})
	foreignChild := h.createChild(foreign, subagent.Link{TaskCallID: "foreign-child"})
	killed := h.createRoot(managerAttrs("owner"))
	h.say(killed, "/kill")
	h.waitUntil("killed", func() bool { return h.session(killed).KilledAt != nil })
	parked := h.createRoot(managerAttrs("owner"))
	fireOrchestratorBudget(t, h, parked)
	stopping := h.createRoot(managerAttrs("owner"))
	_, err := h.db.ExecContext(h.ctx, "UPDATE sessions SET status = 'stopping' WHERE id = ?", stopping)
	require.NoError(t, err)
	for _, tc := range []struct {
		id   int64
		text string
	}{
		{99999, "session #99999 not found"},
		{foreignChild, fmt.Sprintf("session #%d is a subagent, not a root session", foreignChild)},
		{child, fmt.Sprintf("session #%d is a subagent, not a root session", child)},
		{killed, fmt.Sprintf("session #%d is killed", killed)},
		{caller, "cannot send to this session itself"},
		{parked, fmt.Sprintf("session #%d is parked at its budget checkpoint; only the human can resume it", parked)},
		{stopping, fmt.Sprintf("session #%d is stopping; try again shortly", stopping)},
	} {
		_, _, err := (orchestratorHost{h.mgr}).SendToSession(h.ctx, caller, tc.id, "hello")
		require.EqualError(t, err, tc.text)
	}
	_, err = h.store.SetOrchestrator(h.ctx, caller, false)
	require.NoError(t, err)
	_, _, err = (orchestratorHost{h.mgr}).SendToSession(h.ctx, caller, stopping, "hello")
	require.EqualError(t, err, "orchestrator mode is off for this session")
	_, _, err = (orchestratorHost{h.mgr}).ListSessions(h.ctx, caller, "")
	require.EqualError(t, err, "orchestrator mode is off for this session")
	_, _, err = (orchestratorHost{h.mgr}).SpawnSession(h.ctx, caller, orchestrator.SpawnRequest{
		Project: "missing",
		Prompt:  "hello",
	})
	require.EqualError(t, err, "orchestrator mode is off for this session")
}

func TestOrchestratorSendCarriesNoHumanAuthority(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: func(_ string, _ []llmwire.Message) *llmwire.Response {
		return textReply("accepted")
	}})
	h.startInboxWake()
	caller := modeRoot(t, h)
	target := h.createRoot(managerAttrs("owner"))
	require.NoError(t, h.store.SetSessionTitle(h.ctx, target, "unchanged"))
	for _, text := range []string{"/status", "/budget one dollar", "fix the login bug"} {
		resolved, _, err := (orchestratorHost{h.mgr}).SendToSession(h.ctx, caller, target, text)
		require.NoError(t, err)
		assert.Equal(t, target, resolved)
		h.waitUntil("sent text accepted", func() bool {
			return h.countOutbox(target, func(content string) bool {
				return strings.HasPrefix(content, fmt.Sprintf("↪ from #%d ", caller)) &&
					strings.HasSuffix(content, "\n\n"+text)
			}) == 1
		})
		h.waitAnswer(target, "accepted")
		assert.True(t, hasUserContaining(h.messages(target), fmt.Sprintf("[Message from coagent session #%d", caller)))
		assert.True(t, hasUserContaining(h.messages(target), "not by the human]\n\n"+text))
	}
	var humanRows int
	require.NoError(
		t,
		h.db.QueryRowContext(h.ctx, "SELECT count(*) FROM session_inbox WHERE session_id = ? AND source = 'user'", target).
			Scan(&humanRows),
	)
	assert.Zero(t, humanRows)
	assert.Equal(t, "unchanged", h.session(target).Attributes[controllerapi.SessionAttributeTitle])
	parked := h.createRoot(managerAttrs("owner"))
	fireOrchestratorBudget(t, h, parked)
	_, _, err := (orchestratorHost{h.mgr}).SendToSession(h.ctx, caller, parked, "/budget resume")
	require.ErrorContains(t, err, "only the human can resume it")
	record, err := h.store.Get(h.ctx, parked)
	require.NoError(t, err)
	assert.Equal(t, budget.Fired, record.State)
}

func TestOrchestratorSendFollowsClearReplacement(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	h.startInboxWake()
	caller := modeRoot(t, h)
	target := h.createRoot(managerAttrs("owner"))
	h.say(target, "/clear")
	var replacement int64
	h.waitUntil("clear replacement", func() bool {
		var err error
		replacement, err = h.store.ResolveReplacement(h.ctx, target)
		return err == nil && replacement != target
	})
	resolved, _, err := (orchestratorHost{h.mgr}).SendToSession(h.ctx, caller, target, "after clear")
	require.NoError(t, err)
	assert.Equal(t, replacement, resolved)
	h.waitAnswer(replacement, "done")
	assert.True(t, hasUserContaining(h.messages(replacement), "after clear"))
}

func TestOrchestratorToolTrailInOwnChat(t *testing.T) {
	var target int64
	h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "send the message") &&
			!hasToolResultFor(messages, orchestrator.IDSendToSession) {
			return callReply(
				"send-other",
				orchestrator.IDSendToSession,
				fmt.Sprintf(`{"session_id":%d,"message":"hello target"}`, target),
			)
		}
		return textReply("done")
	}})
	h.startInboxWake()
	target = h.createRoot(managerAttrs("owner"))
	caller := modeRoot(t, h)
	h.say(caller, "send the message")
	h.waitUntil("tool trail", func() bool {
		return h.countOutbox(caller, func(content string) bool {
			return strings.Contains(content, "→ send_to_session: ") && strings.Contains(content, "hello target")
		}) == 1
	})
	h.waitAnswer(caller, "done")
}

func modeRoot(t *testing.T, h *harness) int64 {
	t.Helper()
	id := h.createRoot(managerAttrs("owner"))
	_, err := h.store.SetOrchestrator(h.ctx, id, true)
	require.NoError(t, err)
	return id
}

func fireOrchestratorBudget(t *testing.T, h *harness, id int64) {
	t.Helper()
	limit := 1.0
	armed, err := h.store.Arm(
		h.ctx,
		budget.Mutation{RootSessionID: id, ToolCallID: "arm", CostLimitUSD: &limit, Receipt: "armed"},
	)
	require.NoError(t, err)
	_, _, err = h.store.FireBudget(h.ctx, id, armed.Generation, "cost", 2, "checkpoint")
	require.NoError(t, err)
	require.NoError(t, h.store.UpdateSessionStatus(h.ctx, id, sessionstore.SessionStatusStopped))
}

func assertModeToolSchemas(t *testing.T, ids []string, want bool) {
	t.Helper()
	for _, id := range []string{
		orchestrator.IDListSessions, orchestrator.IDSendToSession, orchestrator.IDSpawnSession, orchestrator.IDReadSession,
		orchestrator.IDSubscribe, orchestrator.IDUnsubscribe, orchestrator.IDStopSession, orchestrator.IDKillSession,
	} {
		if want {
			assert.Contains(t, ids, id)
		} else {
			assert.NotContains(t, ids, id)
		}
	}
}
