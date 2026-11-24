package daemon

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestAppSendIsASentMessage(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	h.startInboxWake()
	target := h.createRoot(nil)
	require.NoError(t, h.store.SetSessionTitle(h.ctx, target, "unchanged"))
	for _, text := range []string{"/status", "/stop", "/config", "/skill x", "fix the login bug"} {
		resolved, err := h.mgr.SendForApp(h.ctx, "bot", target, text, "")
		require.NoError(t, err)
		assert.Equal(t, target, resolved)
		h.waitUntil("app sent text accepted", func() bool {
			return h.countOutbox(target, func(content string) bool {
				return content == "↪ from app bot\n\n"+text
			}) == 1 && hasUserContaining(h.messages(target),
				"[Message from the program \"bot\" — sent through the local API, not by the human]\n\n"+text)
		})
		waitSignalTurn(t, h, target, "done")
	}
	var humanRows int
	require.NoError(t, h.db.QueryRowContext(h.ctx,
		"SELECT COUNT(*) FROM session_inbox WHERE session_id = ? AND source = 'user'", target).Scan(&humanRows))
	assert.Zero(t, humanRows)
	assert.Equal(t, "unchanged", h.session(target).Attributes[controllerapi.SessionAttributeTitle])
	parked := h.createRoot(nil)
	fireOrchestratorBudget(t, h, parked)
	_, err := h.mgr.SendForApp(h.ctx, "bot", parked, "/status", "")
	require.EqualError(t, err, fmt.Sprintf(
		"session #%d is parked at its budget checkpoint; only the human can resume it", parked))
	record, err := h.store.Get(h.ctx, parked)
	require.NoError(t, err)
	assert.Equal(t, budget.Fired, record.State)
	killed := h.createRoot(nil)
	h.say(killed, "/kill")
	_, err = h.mgr.SendForApp(h.ctx, "bot", killed, "work", "")
	require.EqualError(t, err, fmt.Sprintf("session #%d is killed", killed))
	child := h.createUnlinkedChild(target)
	_, err = h.mgr.SendForApp(h.ctx, "bot", child, "work", "")
	require.EqualError(t, err, fmt.Sprintf("session #%d is a subagent, not a root session", child))
}

func TestAppStartedRootTakesItsFirstMessageAsTitle(t *testing.T) {
	h := orchestratorProjectHarness(t)
	id, err := h.mgr.SpawnForApp(h.ctx, "bot", h.workDir(), "", "")
	require.NoError(t, err)
	_, err = h.mgr.SendForApp(h.ctx, "bot", id, "first line\nmore", "bot:first")
	require.NoError(t, err)
	assert.Equal(t, "first line", h.session(id).Attributes[controllerapi.SessionAttributeTitle])
	_, err = h.mgr.SendForApp(h.ctx, "bot", id, "second line", "bot:second")
	require.NoError(t, err)
	assert.Equal(t, "first line", h.session(id).Attributes[controllerapi.SessionAttributeTitle])
	_, err = h.mgr.SendForApp(h.ctx, "bot", id, "duplicate", "bot:first")
	require.NoError(t, err)
	assert.Equal(t, "first line", h.session(id).Attributes[controllerapi.SessionAttributeTitle])
	foreign := h.createRoot(nil)
	_, err = h.mgr.SendForApp(h.ctx, "bot", foreign, "not my root", "")
	require.NoError(t, err)
	assert.NotContains(t, h.session(foreign).Attributes, controllerapi.SessionAttributeTitle)
	untitled, err := h.mgr.SpawnForApp(h.ctx, "bot", h.workDir(), "", "")
	require.NoError(t, err)
	require.NoError(t, h.store.SetSessionTitle(h.ctx, untitled, "human title"))
	_, err = h.mgr.SendForApp(h.ctx, "bot", untitled, "app title", "")
	require.NoError(t, err)
	assert.Equal(t, "human title", h.session(untitled).Attributes[controllerapi.SessionAttributeTitle])
}

func TestAppSpawn(t *testing.T) {
	for _, tc := range []struct {
		name, model, wantModel, wantEffort string
	}{
		{name: "default", wantModel: "fake-model", wantEffort: "high"},
		{name: "tagged", model: "fast", wantModel: "fast", wantEffort: "low"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := orchestratorModelHarness(t)
			id, err := h.mgr.SpawnForApp(h.ctx, "bot", h.workDir(), "", tc.model)
			require.NoError(t, err)
			record := h.session(id)
			assert.Equal(t, "bot", controllerapi.SpawnedByApp(record.Attributes))
			assert.Zero(t, controllerapi.SpawnedBy(record.Attributes))
			assert.Equal(t, tc.wantModel, record.Model)
			assert.Equal(t, tc.wantEffort, record.ReasoningLevel)
			assert.False(t, controllerapi.OrchestratorMode(record.Attributes))
			var inputs, watches int
			require.NoError(t, h.db.QueryRowContext(h.ctx,
				"SELECT COUNT(*) FROM session_inbox WHERE session_id = ?", id).Scan(&inputs))
			require.NoError(t, h.db.QueryRowContext(h.ctx,
				"SELECT COUNT(*) FROM session_watches WHERE target_id = ?", id).Scan(&watches))
			assert.Zero(t, inputs)
			assert.Zero(t, watches)
			assert.False(t, h.mgr.HasActiveLoop(id))
			replacement := clearReadTarget(t, h, id)
			assert.Equal(t, "bot", controllerapi.SpawnedByApp(h.session(replacement).Attributes))
			assert.Equal(t, int64(0), h.session(replacement).ParentID)
			assert.NotEqual(t, sessionstore.SessionStatusKilled, h.session(replacement).Status)
			_, err = h.mgr.SendForApp(h.ctx, "bot", replacement, "app task", "")
			require.NoError(t, err)
			h.waitAnswer(replacement, "done")
		})
	}
}
