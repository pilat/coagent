package daemon

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestAppEndsOnlyWhatItStarted(t *testing.T) {
	release := make(chan struct{})
	h := orchestratorProjectHarness(t, func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "queued work") {
			<-release
		}
		return textReply("done")
	})
	t.Cleanup(func() { closeOnce(release) })
	caller := modeRoot(t, h)
	other, err := h.mgr.SpawnForApp(h.ctx, "another", h.workDir(), "", "")
	require.NoError(t, err)
	spawned := spawnEndTarget(t, h, caller, "orchestrator work")
	human := h.createRoot(nil)
	for _, target := range []int64{other, spawned, human} {
		before := endRowCounts(t, h, target)
		want := fmt.Sprintf(
			"session #%d was not started by this app; you can stop or kill only sessions you started", target)
		_, err = h.mgr.StopForApp(h.ctx, "bot", target)
		require.EqualError(t, err, want)
		_, err = h.mgr.KillForApp(h.ctx, "bot", target)
		require.EqualError(t, err, want)
		assert.Equal(t, before, endRowCounts(t, h, target))
	}
	for _, replacement := range []bool{false, true} {
		t.Run(fmt.Sprintf("replacement=%t", replacement), func(t *testing.T) {
			id, err := h.mgr.SpawnForApp(h.ctx, "bot", h.workDir(), "", "")
			require.NoError(t, err)
			target := id
			if replacement {
				target = clearReadTarget(t, h, id)
			}
			_, err = h.mgr.SendForApp(h.ctx, "bot", id, "queued work", "")
			require.NoError(t, err)
			resolved, err := h.mgr.StopForApp(h.ctx, "bot", id)
			require.NoError(t, err)
			assert.Equal(t, target, resolved)
			assert.Equal(t, sessionstore.SessionStatusStopped, h.session(target).Status)
			assert.Equal(t, 1, h.countOutbox(target, func(content string) bool {
				return content == "⏳ Stopping… — asked by app bot"
			}))
			resolved, err = h.mgr.KillForApp(h.ctx, "bot", id)
			require.NoError(t, err)
			assert.Equal(t, target, resolved)
			assert.NotNil(t, h.session(target).KilledAt)
			assert.Equal(t, 1, h.countOutbox(target, func(content string) bool {
				return content == "Stopping session... — asked by app bot"
			}))
		})
	}
}

func TestAppStopSignalsWatchersWithTheApp(t *testing.T) {
	for _, kill := range []bool{false, true} {
		t.Run(fmt.Sprintf("kill=%t", kill), func(t *testing.T) {
			h := orchestratorProjectHarness(t)
			watcher := modeRoot(t, h)
			target, err := h.mgr.SpawnForApp(h.ctx, "bot", h.workDir(), "", "")
			require.NoError(t, err)
			_, _, err = (orchestratorHost{s: h.mgr}).Subscribe(h.ctx, watcher, target)
			require.NoError(t, err)
			_, err = h.mgr.SendForApp(h.ctx, "bot", target, "queued work", "")
			require.NoError(t, err)
			fact, notice := "was stopped by app bot with StopSession", "was stopped by app bot"
			if kill {
				_, err = h.mgr.KillForApp(h.ctx, "bot", target)
				fact, notice = "was killed by app bot with KillSession", "was killed by app bot"
			} else {
				_, err = h.mgr.StopForApp(h.ctx, "bot", target)
			}
			require.NoError(t, err)
			h.startInboxWake()
			assertOutcomeSignal(t, h, watcher, target, target, fact, notice)
		})
	}
}

func TestAppEndCommandSurvivesRestart(t *testing.T) {
	h := orchestratorProjectHarness(t)
	watcher := modeRoot(t, h)
	target, err := h.mgr.SpawnForApp(h.ctx, "bot", h.workDir(), "", "")
	require.NoError(t, err)
	_, err = h.mgr.SendForApp(h.ctx, "bot", target, "queued work", "")
	require.NoError(t, err)
	host := orchestratorHost{s: h.mgr}
	_, _, err = host.Subscribe(h.ctx, watcher, target)
	require.NoError(t, err)
	sender, err := host.provenance(h.ctx, delegate{app: "bot"})
	require.NoError(t, err)
	queued, err := h.store.Enqueue(h.ctx, sessionstore.Input{
		SessionID: target, Source: sessionstore.InputSourceUser, Content: stopCommand, Attributes: sender,
	})
	require.NoError(t, err)
	require.True(t, queued.Applied)
	h = h.restart(trivialRespond)
	h.resume()
	h.waitUntil("recovered app stop complete", func() bool {
		return h.session(target).Status == sessionstore.SessionStatusStopped
	})
	assert.Equal(t, 1, h.countOutbox(target, func(content string) bool {
		return content == "⏳ Stopping… — asked by app bot"
	}))
	assert.Equal(t, 1, h.countOutbox(target, func(content string) bool {
		return content == sessionstore.StopTerminalContent+"\nCancelled background processes: 0"
	}))
	assertOutcomeSignal(t, h, watcher, target, target,
		"was stopped by app bot with StopSession", "was stopped by app bot")
}
