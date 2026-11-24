package daemon

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

// Every lifecycle call revalidates mode, root target and spawning authority before writing.
func TestOrchestratorEndRefusals(t *testing.T) {
	h := orchestratorProjectHarness(t)
	caller, other := modeRoot(t, h), modeRoot(t, h)
	human := h.createRoot(nil)
	foreign := h.createRoot(nil)
	child := h.createUnlinkedChild(caller)
	othersTarget := spawnEndTarget(t, h, other, "another orchestrator's work")
	for _, tc := range []struct {
		name   string
		target int64
		want   string
	}{
		{name: "human root", target: human, want: endOwnershipRefusal(human)},
		{name: "other spawner", target: othersTarget, want: endOwnershipRefusal(othersTarget)},
		{name: "subagent", target: child, want: fmt.Sprintf("session #%d is a subagent, not a root session", child)},
		{name: "another manager's root", target: foreign, want: endOwnershipRefusal(foreign)},
		{name: "self", target: caller, want: endOwnershipRefusal(caller)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertEndRefusals(t, h, caller, tc.target, tc.want)
		})
	}
	owned := spawnEndTarget(t, h, caller, "owned work")
	_, err := h.store.SetOrchestrator(h.ctx, caller, false)
	require.NoError(t, err)
	assertEndRefusals(t, h, caller, owned, "orchestrator mode is off for this session")
}

// Replacement chains retain both the target's spawn fact and the caller's authority over it.
func TestOrchestratorEndFollowsLineage(t *testing.T) {
	release := make(chan struct{})
	h := orchestratorProjectHarness(t, func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasUserContaining(messages, "BLOCK_TARGET") {
			<-release
		}
		return textReply("done")
	})
	defer closeOnce(release)
	caller := modeRoot(t, h)
	target := spawnEndTarget(t, h, caller, "target work")
	replacement := clearReadTarget(t, h, target)
	h.say(replacement, "BLOCK_TARGET")
	h.waitUntil("replacement running", func() bool {
		return h.mgr.HasActiveLoop(replacement) && hasUserContaining(h.messages(replacement), "BLOCK_TARGET")
	})
	host := orchestratorHost{s: h.mgr}
	ended, err := host.StopSession(h.ctx, caller, target)
	require.NoError(t, err)
	assert.Equal(t, replacement, ended.ID)
	assert.True(t, ended.Changed)
	assert.Equal(t, sessionstore.SessionStatusStopped, h.session(replacement).Status)
	human := h.createRoot(nil)
	assertListedSpawned(t, h, caller, map[int64]bool{replacement: true, human: false})
	child := spawnEndTarget(t, h, caller, "survives caller clear")
	callerReplacement := clearReadTarget(t, h, caller)
	assertListedSpawned(t, h, callerReplacement, map[int64]bool{replacement: true, child: true, human: false})
	ended, err = host.KillSession(h.ctx, callerReplacement, child)
	require.NoError(t, err)
	assert.Equal(t, child, ended.ID)
	assert.True(t, ended.Changed)
	assert.NotNil(t, h.session(child).KilledAt)
	assertOrphanedSpawnRefused(t, h)
}

func assertEndRefusals(t *testing.T, h *harness, caller, target int64, want string) {
	t.Helper()
	before := endRowCounts(t, h, target)
	host := orchestratorHost{s: h.mgr}
	_, err := host.StopSession(h.ctx, caller, target)
	require.EqualError(t, err, want)
	assert.Equal(t, before, endRowCounts(t, h, target))
	_, err = host.KillSession(h.ctx, caller, target)
	require.EqualError(t, err, want)
	assert.Equal(t, before, endRowCounts(t, h, target))
}

func endOwnershipRefusal(target int64) string {
	return fmt.Sprintf(
		"session #%d was not started by this session; you can stop or kill only sessions you spawned",
		target,
	)
}

func assertListedSpawned(t *testing.T, h *harness, caller int64, want map[int64]bool) {
	t.Helper()
	entries, _, err := (orchestratorHost{s: h.mgr}).ListSessions(h.ctx, caller, "")
	require.NoError(t, err)
	found := make(map[int64]bool)
	for _, entry := range entries {
		if _, listed := want[entry.ID]; listed {
			found[entry.ID] = entry.Spawned
		}
	}
	assert.Equal(t, want, found)
}

func assertOrphanedSpawnRefused(t *testing.T, h *harness) {
	t.Helper()
	spawner := modeRoot(t, h)
	orphan := spawnEndTarget(t, h, spawner, "orphan work")
	h.say(spawner, "/kill")
	require.NotNil(t, h.session(spawner).KilledAt)
	waitSignalTurn(t, h, orphan, "done")
	assert.Nil(t, h.session(orphan).KilledAt)
	third := modeRoot(t, h)
	assertEndRefusals(t, h, third, orphan, endOwnershipRefusal(orphan))
	assertListedSpawned(t, h, third, map[int64]bool{orphan: false})
}

// A committed command recovers through the human flow with its durable sender and one outcome signal.
func TestOrchestratorEndCommandSurvivesRestart(t *testing.T) {
	h := orchestratorProjectHarness(t)
	caller, watcher := modeRoot(t, h), modeRoot(t, h)
	require.NoError(t, h.store.SetSessionTitle(h.ctx, caller, "stop the queued target"))
	target := spawnEndTarget(t, h, caller, "queued target")
	host := orchestratorHost{s: h.mgr}
	_, _, err := host.Subscribe(h.ctx, watcher, target)
	require.NoError(t, err)
	sender, err := host.provenance(h.ctx, h.session(caller))
	require.NoError(t, err)
	queued, err := h.store.Enqueue(h.ctx, sessionstore.Input{
		SessionID:  target,
		Source:     sessionstore.InputSourceUser,
		Content:    stopCommand,
		Attributes: sender,
	})
	require.NoError(t, err)
	require.True(t, queued.Applied)
	h = h.restart(trivialRespond)
	h.resume()
	h.waitUntil("recovered stop complete", func() bool {
		return h.session(target).Status == sessionstore.SessionStatusStopped
	})
	ack := fmt.Sprintf("⏳ Stopping… — asked by #%d %s", caller, sender[sessionstore.InputAttributeSenderName])
	assert.Equal(t, 1, h.countOutbox(target, func(content string) bool { return content == ack }))
	assert.Equal(t, 1, h.countOutbox(target, func(content string) bool {
		return content == sessionstore.StopTerminalContent+"\nCancelled background processes: 0"
	}))
	assertOutcomeSignal(t, h, watcher, target, target,
		fmt.Sprintf("was stopped by session #%d with stop_session", caller), fmt.Sprintf("was stopped by #%d", caller))
	assert.Zero(t, signalRows(t, h, caller, target))
	assert.Equal(t, caller, controllerapi.SpawnedBy(h.session(target).Attributes))
}
