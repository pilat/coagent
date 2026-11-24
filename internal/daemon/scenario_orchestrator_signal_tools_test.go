package daemon

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

func TestOrchestratorSubscribeRefusals(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	h.startInboxWake()
	watcher, target := modeRoot(t, h), modeRoot(t, h)
	foreign := h.createUnlinkedChild(h.createRoot(managerAttrs("other")))
	child := h.createChild(watcher, subagent.Link{TaskCallID: "subscribe-child"})
	killed := h.createRoot(managerAttrs("owner"))
	h.say(killed, "/kill")
	h.waitUntil("killed", func() bool { return h.session(killed).KilledAt != nil })
	host := orchestratorHost{s: h.mgr}
	for _, tc := range []struct {
		id   int64
		text string
	}{
		{id: 99999, text: "session #99999 not found"},
		{id: foreign, text: fmt.Sprintf("session #%d is a subagent, not a root session", foreign)},
		{id: child, text: fmt.Sprintf("session #%d is a subagent, not a root session", child)},
		{id: killed, text: fmt.Sprintf("session #%d is killed", killed)},
		{id: watcher, text: "cannot subscribe to this session itself"},
	} {
		_, _, err := host.Subscribe(h.ctx, watcher, tc.id)
		require.EqualError(t, err, tc.text)
	}
	_, _, err := host.Subscribe(h.ctx, target, watcher)
	require.NoError(t, err)
	_, _, err = host.Subscribe(h.ctx, watcher, target)
	require.EqualError(
		t,
		err,
		fmt.Sprintf(
			"session #%d is already subscribed to this session; two sessions cannot subscribe to each other",
			target,
		),
	)
	id, was, err := host.Unsubscribe(h.ctx, target, watcher)
	require.NoError(t, err)
	assert.Equal(t, watcher, id)
	assert.True(t, was)
	_, was, err = host.Unsubscribe(h.ctx, target, watcher)
	require.NoError(t, err)
	assert.False(t, was)
	replacement := clearReadTarget(t, h, target)
	id, _, err = host.Subscribe(h.ctx, watcher, target)
	require.NoError(t, err)
	assert.Equal(t, replacement, id)
	_, err = h.store.SetOrchestrator(h.ctx, watcher, false)
	require.NoError(t, err)
	_, _, err = host.Subscribe(h.ctx, watcher, target)
	require.EqualError(t, err, "orchestrator mode is off for this session")
	_, _, err = host.Unsubscribe(h.ctx, watcher, target)
	require.EqualError(t, err, "orchestrator mode is off for this session")
}

func TestOrchestratorListShowsWatchState(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, harnessOptions{respond: func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "BLOCK_TARGET") {
			<-release
		}
		return textReply("done")
	}})
	defer closeOnce(release)
	h.startInboxWake()
	watcher := modeRoot(t, h)
	standing := h.createRoot(managerAttrs("owner"))
	awaiting := h.createRoot(managerAttrs("owner"))
	unread := h.createRoot(managerAttrs("owner"))
	h.say(watcher, "/stop")
	h.waitUntil(
		"watcher stopped",
		func() bool { return h.session(watcher).Status == sessionstore.SessionStatusStopped },
	)
	host := orchestratorHost{s: h.mgr}
	_, _, err := host.Subscribe(h.ctx, watcher, standing)
	require.NoError(t, err)
	_, _, err = host.SendToSession(h.ctx, watcher, awaiting, "BLOCK_TARGET")
	require.NoError(t, err)
	_, _, err = host.SendToSession(h.ctx, watcher, unread, "finish")
	require.NoError(t, err)
	h.waitAnswer(unread, "done")
	h.waitUntil("unread signal", func() bool { return signalRows(t, h, watcher, unread) == 1 })
	entries, _, err := host.ListSessions(h.ctx, watcher, "")
	require.NoError(t, err)
	for _, entry := range entries {
		assert.Equal(t, entry.ID == standing, entry.Subscribed)
		assert.Equal(t, entry.ID == awaiting, entry.Armed)
		assert.Equal(t, entry.ID == unread, entry.Signalled)
	}
}
