package daemon

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/orchestrator"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

type readFocus struct {
	manager, chat   string
	session, cursor sql.NullInt64
}

type readObservation struct {
	inbox   int
	record  *sessionstore.SessionRecord
	outputs []outboxRow
	focus   []readFocus
}

func TestOrchestratorReadSessionRefusals(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	h.startInboxWake()
	caller := modeRoot(t, h)
	foreign := h.createRoot(managerAttrs("other"))
	child := h.createChild(caller, subagent.Link{TaskCallID: "owned-read-child"})
	foreignChild := h.createChild(foreign, subagent.Link{TaskCallID: "foreign-read-child"})
	for _, tc := range []struct {
		id   int64
		text string
	}{
		{id: 99999, text: "session #99999 not found"},
		{id: foreign, text: fmt.Sprintf("session #%d not found", foreign)},
		{id: foreignChild, text: fmt.Sprintf("session #%d not found", foreignChild)},
		{id: child, text: fmt.Sprintf("session #%d is a subagent, not a root session", child)},
		{id: caller, text: "cannot read this session itself"},
	} {
		_, err := (orchestratorHost{s: h.mgr}).ReadSession(h.ctx, caller, tc.id, false)
		require.EqualError(t, err, tc.text)
	}
	_, err := h.store.SetOrchestrator(h.ctx, "owner", caller, false)
	require.NoError(t, err)
	_, err = (orchestratorHost{s: h.mgr}).ReadSession(h.ctx, caller, 99999, false)
	require.EqualError(t, err, "orchestrator mode is off for this session")
}

func TestOrchestratorReadSessionFollowsTargetTurns(t *testing.T) {
	turns := scriptTurns{
		{
			marker: "FIRST_TURN",
			calls:  []scriptCall{{id: "read-target-ls", name: "ls", params: `{"path":"."}`}},
			answer: "first answer",
		},
		{marker: "SENT_TURN", answer: "sent answer"},
	}
	h := newHarness(t, harnessOptions{respond: turns.respond})
	h.startInboxWake()
	caller := modeRoot(t, h)
	target := h.createRoot(managerAttrs("owner"))
	h.reply(target, "FIRST_TURN", "first answer")
	seedReadFocus(t, h, target)
	page := readWithoutTargetChanges(t, h, caller, target)
	assert.Equal(t, []string{"step", "answer"}, readKinds(page))
	assert.Equal(t, "idle", page.State)
	assert.Contains(t, page.Rows[0].Content, "→ ls: `.`")
	assert.Equal(t, "first answer", page.Rows[1].Content)
	page = readWithoutTargetChanges(t, h, caller, target)
	assert.Empty(t, page.Rows)
	_, _, err := (orchestratorHost{s: h.mgr}).SendToSession(h.ctx, caller, target, "SENT_TURN")
	require.NoError(t, err)
	h.waitAnswer(target, "sent answer")
	page = readWithoutTargetChanges(t, h, caller, target)
	require.Equal(t, []string{"sent", "answer"}, readKinds(page))
	assert.True(t, strings.HasPrefix(page.Rows[0].Content, fmt.Sprintf("↪ from #%d ", caller)))
	assert.Equal(t, "sent answer", page.Rows[1].Content)
}

func seedReadFocus(t *testing.T, h *harness, target int64) {
	t.Helper()
	require.NoError(t, h.store.BindChat(h.ctx, "owner", "focused"))
	require.NoError(t, h.store.SetChatFocus(h.ctx, "owner", "focused", target))
	outputs := h.outbox(target)
	require.NotEmpty(t, outputs)
	entered, err := h.store.EnterChat(h.ctx, "owner", "focused", target, outputs[0].ID)
	require.NoError(t, err)
	require.True(t, entered)
	require.NoError(t, h.store.BindChat(h.ctx, "other", "unfocused"))
}

func readWithoutTargetChanges(t *testing.T, h *harness, caller, target int64) orchestrator.Page {
	t.Helper()
	before := observeReadTarget(t, h, target)
	page, err := (orchestratorHost{s: h.mgr}).ReadSession(h.ctx, caller, target, false)
	require.NoError(t, err)
	assert.Equal(t, before, observeReadTarget(t, h, target), "reading must not alter the target or any human cursor")
	return page
}

func observeReadTarget(t *testing.T, h *harness, target int64) readObservation {
	t.Helper()
	state := readObservation{record: h.session(target), outputs: h.outbox(target)}
	require.NoError(t, h.db.QueryRowContext(h.ctx,
		`SELECT COUNT(*) FROM session_inbox WHERE session_id = ?`, target).Scan(&state.inbox))
	rows, err := h.db.QueryContext(h.ctx,
		`SELECT manager_id, chat_id, session_id, last_sent_id FROM chat_focus ORDER BY manager_id, chat_id`)
	require.NoError(t, err)
	defer rows.Close()
	for rows.Next() {
		var focus readFocus
		require.NoError(t, rows.Scan(&focus.manager, &focus.chat, &focus.session, &focus.cursor))
		state.focus = append(state.focus, focus)
	}
	require.NoError(t, rows.Err())
	return state
}

func readKinds(page orchestrator.Page) []string {
	var kinds []string
	for _, row := range page.Rows {
		kinds = append(kinds, row.Kind)
	}
	return kinds
}
