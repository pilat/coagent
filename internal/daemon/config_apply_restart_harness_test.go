package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

const applyCallID = "cfg-call-1"

// applyDaemon is one daemon "process image" over a shared database and a shared
// config directory, so a test can take one down and bring the next one up on the
// same durable state the way a restart-apply does.
type applyDaemon struct {
	*subagentHarness
	ops      configops.Service
	restarts chan struct{}
}

// configApplyRespond calls one config tool, then answers once its verdict is in
// the transcript. A second call would mean the suspended call was re-executed.
func configApplyRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if hasToolResultFor(msgs, tool.IDSetDefaultModel) {
		return &llmwire.Response{Text: "default model switched"}
	}

	return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
		ID:        applyCallID,
		Name:      tool.IDSetDefaultModel,
		Arguments: []byte(`{"id":"claude-opus-5"}`),
	}}}
}

func newApplyConfigDir(t *testing.T) string {
	t.Helper()

	return newApplyConfigDirWith(t, toolConfig)
}

func newApplyConfigDirWith(t *testing.T, configYAML string) string {
	t.Helper()

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(configYAML), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secrets"), []byte(toolSecrets), 0o600))

	return dir
}

func newApplyDaemon(t *testing.T, dbPath, configDir string) *applyDaemon {
	t.Helper()

	return newApplyDaemonWith(t, dbPath, configDir, configApplyRespond)
}

func (d *applyDaemon) restartCount() int { return len(d.restarts) }

func (d *applyDaemon) waitForRestart(t *testing.T) {
	t.Helper()

	select {
	case <-d.restarts:
	case <-time.After(5 * time.Second):
		t.Fatal("the staged config change never asked for a restart")
	}
}

// bootVerdict replays what cmd/coagent's boot does with a marker: resolve it,
// then hand the verdict to the session that suspended.
func (d *applyDaemon) bootVerdict(t *testing.T) (configops.Outcome, error) {
	t.Helper()

	pending, err := d.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending, "the boot after an apply must still find the marker")

	outcome, err := d.ops.ResolvePending(*pending, nil)
	require.NoError(t, err)

	message := "Config applied: " + outcome.Pending.Summary
	if outcome.Verdict.Failed() {
		message = "Config change rejected — " + outcome.Verdict.Reason()
	}

	if _, err := d.mgr.DeliverPendingCallResult(
		d.ctx, outcome.Pending.SessionID, outcome.Pending.ToolCallID, outcome.Pending.ToolName, message,
	); err != nil {
		return outcome, err
	}

	return outcome, d.ops.ClearPending(outcome.Pending)
}

// stageApplyAndStop runs a session up to the suspend the config tool causes,
// commits the change, and takes the daemon down the way the restart would.
func stageApplyAndStop(t *testing.T, dbPath, configDir string) int64 {
	t.Helper()

	first := newApplyDaemon(t, dbPath, configDir)

	sessionID, err := first.mgr.Send(
		first.ctx, first.projectID, "switch the default model", "fake-model", map[string]any{"channel": "cli"},
	)
	require.NoError(t, err)

	first.waitForRestart(t)
	first.waitUntil("session suspended on the config call", func() bool {
		return !first.mgr.HasActiveLoop(sessionID)
	})

	pending, err := first.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending, "the commit leaves a marker naming the waiting session")
	require.Equal(t, sessionID, pending.SessionID)
	require.Equal(t, applyCallID, pending.ToolCallID)
	require.Equal(t, tool.IDSetDefaultModel, pending.ToolName)

	msgs := first.parentMessages(sessionID)
	require.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDSetDefaultModel))
	require.Zero(t, countToolResultsFor(msgs, tool.IDSetDefaultModel), "the call is out with the world")

	first.shutdown()

	return sessionID
}
