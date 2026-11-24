package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
)

func TestHarnessScenario_TwoSurfacesDoubleNothingDurable(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	h.startInboxWake()
	controllers := newTestController(h.mgr)
	alpha := controllers.ForManager("alpha")
	beta := controllers.ForManager("beta")
	for _, control := range []controllerapi.Controller{alpha, beta} {
		subscription := control.Subscribe()
		t.Cleanup(func() { control.Unsubscribe(subscription) })
		chat := control.(controllerapi.ChatFocusController)
		require.NoError(t, chat.BindChat(h.ctx, "chat"))
	}
	watcher := modeRoot(t, h)
	target, err := alpha.CreateSession(h.ctx, controllerapi.SessionCreateData{
		WorkDir: h.workDir(), Model: "fake-model",
	})
	require.NoError(t, err)
	for _, control := range []controllerapi.Controller{alpha, beta} {
		require.NoError(t, control.(controllerapi.ChatFocusController).SetChatFocus(h.ctx, "chat", target))
	}
	_, _, err = (orchestratorHost{s: h.mgr}).Subscribe(h.ctx, watcher, target)
	require.NoError(t, err)
	input := controllerapi.SessionMessageData{SessionID: target, Message: "first", IdempotencyKey: "k-1"}
	require.NoError(t, beta.SendSessionMessage(h.ctx, input))
	h.waitAnswer(target, "done")
	assertAnswerSignal(t, h, watcher, target)
	h.waitAnswer(watcher, "done")
	require.NoError(t, alpha.SendSessionMessage(h.ctx, input))
	applied, err := enqueueScheduledInput(h.ctx, h.schedules, target, "scheduled", false)
	require.NoError(t, err)
	require.True(t, applied)
	done := func(content string) bool { return content == "done" }
	h.waitUntil("two target answers", func() bool { return h.countOutbox(target, done) == 2 })
	var users, schedules, roots int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT
		(SELECT COUNT(*) FROM session_inbox WHERE session_id = ? AND source = 'user'),
		(SELECT COUNT(*) FROM session_inbox WHERE session_id = ? AND source = 'schedule'),
		(SELECT COUNT(*) FROM sessions WHERE parent_id = 0)`, target, target).Scan(&users, &schedules, &roots))
	assert.Equal(t, 1, users)
	assert.Equal(t, 1, schedules)
	assert.Equal(t, 1, signalRows(t, h, watcher, target))
	assert.Equal(t, 2, h.countOutbox(target, done))
	assert.Equal(t, 2, roots)
	rows, err := h.db.QueryContext(h.ctx, `SELECT manager_id, chat_id, session_id FROM chat_focus ORDER BY manager_id`)
	require.NoError(t, err)
	defer rows.Close()
	var chats []string
	for rows.Next() {
		var manager, chat string
		var session int64
		require.NoError(t, rows.Scan(&manager, &chat, &session))
		chats = append(chats, manager)
		assert.Equal(t, "chat", chat)
		assert.Equal(t, target, session)
	}
	require.NoError(t, rows.Err())
	assert.Equal(t, []string{"alpha", "beta"}, chats)
}
