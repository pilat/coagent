package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

const (
	applyCallA = "cfg-call-a"
	applyCallB = "cfg-call-b"
)

// twoSessionApplyRespond drives two root sessions that both reach for the same
// config knob. The prompt tells them apart and the call ids differ, so a call
// left unanswered is visible in the transcript it belongs to.
func twoSessionApplyRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if hasToolResultFor(msgs, tool.IDSetDefaultModel) {
		return &llmwire.Response{Text: "default model handled"}
	}

	if hasUserContaining(msgs, "APPLY_B") {
		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:        applyCallB,
			Name:      tool.IDSetDefaultModel,
			Arguments: []byte(`{"id":"claude-sonnet-5"}`),
		}}}
	}

	return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
		ID:        applyCallA,
		Name:      tool.IDSetDefaultModel,
		Arguments: []byte(`{"id":"claude-opus-5"}`),
	}}}
}

func newApplyDaemonWith(
	t *testing.T,
	dbPath, configDir string,
	respond func(system string, msgs []llmwire.Message) *llmwire.Response,
) *applyDaemon {
	t.Helper()

	h := newSubagentHarnessOnSystemProjectDB(t, dbPath, respond)
	ops := configops.New(filepath.Join(configDir, "config.yaml"), filepath.Join(configDir, "secrets"))
	restarts := make(chan struct{}, 4)

	h.mgr.applier = NewConfigApplier(ops, func() { restarts <- struct{}{} })

	return &applyDaemon{subagentHarness: h, ops: ops, restarts: restarts}
}

func defaultModelInFile(t *testing.T, configDir string) string {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(configDir, "config.yaml"))
	require.NoError(t, err)

	opus := strings.Index(string(body), "id: claude-opus-5")
	sonnet := strings.Index(string(body), "id: claude-sonnet-5")
	require.NotEqual(t, -1, opus)
	require.NotEqual(t, -1, sonnet)

	if opus < sonnet {
		return "claude-opus-5"
	}

	return "claude-sonnet-5"
}

// The marker, the config file and the restart are global, so the "one staged
// change at a time" guard has to be too. A second session's commit would
// overwrite the marker naming the first — losing that session's change and
// leaving its call with no producer that owes it a result.
func TestScenario_ASecondSessionCannotOverwriteAStagedApply(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDir(t)

	first := newApplyDaemonWith(t, dbPath, configDir, twoSessionApplyRespond)

	sessionA, err := first.mgr.Send(
		first.ctx, first.projectID, "APPLY_A switch the default model", "fake-model", map[string]any{"channel": "cli"},
	)
	require.NoError(t, err)

	first.waitForRestart(t)
	first.waitUntil("A suspended on its config call", func() bool { return !first.mgr.HasActiveLoop(sessionA) })

	// B stages against the config A is already restarting into.
	sessionB, err := first.mgr.Send(
		first.ctx, first.projectID, "APPLY_B switch the default model", "fake-model", map[string]any{"channel": "cli"},
	)
	require.NoError(t, err)

	first.mgr.waitIdle(sessionB)

	pending, err := first.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, sessionA, pending.SessionID, "the marker still names the session that is owed a verdict")
	assert.Equal(t, applyCallA, pending.ToolCallID)
	assert.Equal(t, "claude-opus-5", defaultModelInFile(t, configDir), "A's change is the one on disk")
	assert.Zero(t, first.restartCount(), "a refused stage never asks for a second restart")

	msgsB := first.parentMessages(sessionB)
	require.NoError(t, llm.ValidateToolPairing(msgsB))
	assert.Equal(t, 1, countToolResultsFor(msgsB, tool.IDSetDefaultModel), "B is answered in-process")
	assert.Contains(t, lastToolResultContent(msgsB, tool.IDSetDefaultModel), "config change")

	first.shutdown()

	second := newApplyDaemonWith(t, dbPath, configDir, twoSessionApplyRespond)
	defer second.shutdown()

	require.NoError(t, second.mgr.Start(second.ctx))

	outcome, err := second.bootVerdict(t)
	require.NoError(t, err)
	require.True(t, outcome.Verdict.Applied, outcome.Verdict.Reason())

	second.mgr.waitIdle(sessionA)

	msgsA := second.parentMessages(sessionA)
	require.NoError(t, llm.ValidateToolPairing(msgsA))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgsA, tool.IDSetDefaultModel))
	assert.Equal(t, 1, countToolResultsFor(msgsA, tool.IDSetDefaultModel), "A's call is resolved exactly once")
	assert.Contains(t, lastToolResultContent(msgsA, tool.IDSetDefaultModel), "Config applied")
}

// The boot decides "this verdict can never be delivered" from the session
// record. This is the daemon-side half of that contract: the refusal, and the
// record that explains it.
func TestScenario_AVerdictOwedToAKilledSessionIsRefused(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDir(t)

	sessionID := stageApplyAndStop(t, dbPath, configDir)

	second := newApplyDaemon(t, dbPath, configDir)
	defer second.shutdown()

	require.NoError(t, second.mgr.Start(second.ctx))
	require.NoError(t, second.mgr.Kill(second.ctx, sessionID))

	pending, err := second.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)

	_, err = second.mgr.DeliverPendingCallResult(
		second.ctx, pending.SessionID, pending.ToolCallID, pending.ToolName, "Config applied",
	)
	require.Error(t, err, "a killed session can never take the verdict")

	rec, err := second.mgr.GetSession(second.ctx, sessionID)
	require.NoError(t, err)
	assert.NotNil(t, rec.KilledAt, "the record is what the boot reads to tell 'never' from 'not now'")
}

// Same invariant without the staging: whichever session wins the race owns the
// only marker, the loser is refused in-process, and neither transcript is left
// with a dangling tool call.
func TestScenario_ConcurrentAppliesResolveExactlyOnce(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDir(t)

	first := newApplyDaemonWith(t, dbPath, configDir, twoSessionApplyRespond)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		sessions = map[string]int64{}
		sendErrs []error
	)

	for _, prompt := range []string{"APPLY_A switch the default model", "APPLY_B switch the default model"} {
		wg.Go(func() {
			id, err := first.mgr.Send(
				first.ctx, first.projectID, prompt, "fake-model", map[string]any{"channel": "cli"},
			)

			mu.Lock()
			sessions[prompt[:7]] = id
			sendErrs = append(sendErrs, err)
			mu.Unlock()
		})
	}

	wg.Wait()
	require.NoError(t, errors.Join(sendErrs...))

	sessionA, sessionB := sessions["APPLY_A"], sessions["APPLY_B"]

	first.waitForRestart(t)
	first.waitUntil("both sessions settled", func() bool {
		return !first.mgr.HasActiveLoop(sessionA) && !first.mgr.HasActiveLoop(sessionB)
	})

	assert.Zero(t, first.restartCount(), "one apply, one restart")

	pending, err := first.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)

	winner, loser, winnerCall := sessionA, sessionB, applyCallA
	if pending.SessionID == sessionB {
		winner, loser, winnerCall = sessionB, sessionA, applyCallB
	}

	require.Equal(t, winner, pending.SessionID)
	assert.Equal(t, winnerCall, pending.ToolCallID)

	msgsLoser := first.parentMessages(loser)
	require.NoError(t, llm.ValidateToolPairing(msgsLoser))
	assert.Equal(t, 1, countToolResultsFor(msgsLoser, tool.IDSetDefaultModel),
		"the refused call is answered rather than suspended")

	first.shutdown()

	second := newApplyDaemonWith(t, dbPath, configDir, twoSessionApplyRespond)
	defer second.shutdown()

	require.NoError(t, second.mgr.Start(second.ctx))

	_, err = second.bootVerdict(t)
	require.NoError(t, err)

	second.mgr.waitIdle(winner)

	msgsWinner := second.parentMessages(winner)
	require.NoError(t, llm.ValidateToolPairing(msgsWinner))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgsWinner, tool.IDSetDefaultModel))
	assert.Equal(t, 1, countToolResultsFor(msgsWinner, tool.IDSetDefaultModel))
}
