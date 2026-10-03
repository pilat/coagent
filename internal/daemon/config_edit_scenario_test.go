package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

// configEditCallID is the call id the responder always uses, so the
// redelivery test can replay the exact same verdict.
const configEditCallID = "cfg-edit-call-1"

const configEditCandidate = `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
models:
    - id: claude-opus-5
      provider: work
    - id: claude-sonnet-5
      provider: work
`

func TestScenario_AbandonedApplySettlesWithoutRestartingFailedLoop(t *testing.T) {
	testAbandonedApply(t, "")
}

func TestScenario_AbandonedApplyRetriesFailedResultOnExplicitInput(t *testing.T) {
	testAbandonedApply(t, "write failure")
}

func TestScenario_AbandonedApplyRespectsCancellation(t *testing.T) {
	for _, action := range []string{"stop", "kill"} {
		t.Run(action, func(t *testing.T) { testAbandonedApply(t, action) })
	}
}

func TestScenario_AbandonedConfigCommandExpiresGrantBeforeNextInput(t *testing.T) {
	testAbandonedConfigCommand(t, false)
}

func TestScenario_AbandonedConfigCommandRecoversAfterResultFailureAndRestart(t *testing.T) {
	testAbandonedConfigCommand(t, true)
}

func testAbandonedConfigCommand(t *testing.T, restart bool) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "abandoned-command.db")
	configDir := newApplyConfigDir(t)
	d := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
	defer func() { d.shutdown() }()
	d.startInboxWake()
	id, err := d.mgr.Send(d.ctx, d.projectID, "hello", "fake-model", map[string]any{"manager_id": "telegram:main"})
	require.NoError(t, err)
	d.mgr.waitIdle(id)
	_, err = d.db.ExecContext(
		d.ctx,
		`CREATE TRIGGER reject_config_suspend BEFORE UPDATE OF status ON sessions WHEN NEW.status = 'suspended' BEGIN SELECT RAISE(ABORT, 'injected suspend failure'); END`,
	)
	require.NoError(t, err)
	if restart {
		_, err = d.db.ExecContext(d.ctx, `CREATE TRIGGER reject_apply_result BEFORE INSERT ON messages
			WHEN NEW.role = 'tool' AND NEW.tool_name = 'config_edit'
			BEGIN SELECT RAISE(ABORT, 'injected result write failure'); END`)
		require.NoError(t, err)
	}
	require.NoError(t, d.mgr.SendToSession(d.ctx, id, "/config change the default model"))
	d.mgr.waitIdle(id)
	d.waitUntil(
		"abandoned config activation expired",
		func() bool { return currentActivationOf(t, d.subagentHarness, id) == nil },
	)
	if !restart {
		d.waitUntil(
			"abandoned config result settled",
			func() bool { return hasToolResultFor(d.parentMessages(id), tool.IDConfigEdit) },
		)
	}

	activation := currentActivationOf(t, d.subagentHarness, id)
	assert.Nil(t, activation)
	if restart {
		assert.False(t, hasToolResultFor(d.parentMessages(id), tool.IDConfigEdit))
		_, err = d.db.ExecContext(d.ctx, "DROP TRIGGER reject_apply_result")
		require.NoError(t, err)
		_, err = d.db.ExecContext(d.ctx, "DROP TRIGGER reject_config_suspend")
		require.NoError(t, err)
		d.shutdown()
		d = newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
		require.NoError(t, d.mgr.Start(d.ctx))
		d.mgr.waitIdle(id)
	} else {
		_, err = d.db.ExecContext(d.ctx, "DROP TRIGGER reject_config_suspend")
		require.NoError(t, err)
		assert.Contains(t, lastToolResultContent(d.parentMessages(id), tool.IDConfigEdit), "Config change abandoned")
	}
	require.NoError(t, d.mgr.SendToSession(d.ctx, id, "continue after cancellation"))
	d.mgr.waitIdle(id)
	assert.True(t, hasUserContaining(d.parentMessages(id), "continue after cancellation"))
	assert.Equal(t, 1, countToolResultsFor(d.parentMessages(id), tool.IDConfigEdit))
	assert.Zero(t, d.restartCount())
}

func testAbandonedApply(t *testing.T, failure string) {
	t.Helper()
	d := newApplyDaemonWith(t, filepath.Join(t.TempDir(), "abandoned.db"), newApplyConfigDir(t),
		func(string, []llmwire.Message) *llmwire.Response { return &llmwire.Response{Text: "ready"} })
	defer d.shutdown()
	d.startInboxWake()
	id, err := d.mgr.Send(d.ctx, d.projectID, "hello", "fake-model", nil)
	require.NoError(t, err)
	d.mgr.waitIdle(id)
	calls, err := json.Marshal([]llmwire.ToolCall{{ID: configEditCallID, Name: tool.IDConfigEdit}})
	require.NoError(t, err)
	_, err = d.sessStore.Commit(d.ctx, sessionstore.Commit{SessionID: id, Messages: []*transcript.Message{{
		Role: llmwire.RoleAssistant, ToolCalls: calls,
	}}})
	require.NoError(t, err)
	_, err = configapply.NewConfigEdit(id, d.mgr.applier).Execute(
		grantedCall(d.ctx, id, configEditCallID), configEditArgs(configEditCandidate),
	)
	require.ErrorIs(t, err, tool.ErrSuspend)
	configuredModels := d.mgr.buildInput.Config.UnifiedConfig.Models
	d.mgr.buildInput.Config.UnifiedConfig.Models = nil
	failWrite := failure == "write failure"
	if failWrite {
		_, err = d.db.ExecContext(d.ctx, `CREATE TRIGGER reject_apply_result BEFORE INSERT ON messages
			WHEN NEW.role = 'tool' AND NEW.tool_name = 'config_edit'
			BEGIN SELECT RAISE(ABORT, 'injected result write failure'); END`)
		require.NoError(t, err)
	}
	require.NoError(t, d.mgr.SendToSession(d.ctx, id, "keep this input"))
	if failure == "stop" || failure == "kill" {
		if failure == "stop" {
			require.NoError(t, d.mgr.Stop(d.ctx, id, 0))
			assert.Contains(t, lastToolResultContent(d.parentMessages(id), tool.IDConfigEdit), "Stopped by user")
		} else {
			require.NoError(t, d.mgr.Kill(d.ctx, id))
			assert.False(t, hasToolResultFor(d.parentMessages(id), tool.IDConfigEdit))
		}
		assert.Zero(t, d.restartCount())
		return
	}
	d.mgr.waitIdle(id)
	d.waitUntil("failed construction settled its staged apply", func() bool {
		record, err := d.sessStore.GetSession(d.ctx, id)

		return err == nil && record.Status == sessionstore.SessionStatusError &&
			!d.mgr.HasActiveLoop(id) && (failWrite || !d.mgr.applier.Has(id))
	})

	pending, err := d.sessStore.PeekPending(d.ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "keep this input", pending.RawContent)
	if failWrite {
		assert.True(t, d.mgr.applier.Has(id))
		assert.False(t, hasToolResultFor(d.parentMessages(id), tool.IDConfigEdit))
		_, err = d.db.ExecContext(d.ctx, "DROP TRIGGER reject_apply_result")
		require.NoError(t, err)

		d.mgr.buildInput.Config.UnifiedConfig.Models = configuredModels
		require.NoError(t, d.mgr.SendToSession(d.ctx, id, "retry now"))
		d.mgr.waitIdle(id)
		_, err = d.sessStore.PeekPending(d.ctx, id)
		require.ErrorIs(t, err, sessionstore.ErrNoPendingInput)
		assert.True(t, hasUserContaining(d.parentMessages(id), "keep this input"))
	}
	assert.False(t, d.mgr.applier.Has(id))
	assert.Contains(t, lastToolResultContent(d.parentMessages(id), tool.IDConfigEdit), "Config change abandoned")
	assert.Zero(t, d.restartCount())
	apply, err := d.ops.LoadPending()
	require.NoError(t, err)
	assert.Nil(t, apply)
}

// configEditRespond drives the authorized two-turn script: the opener turn
// answers with text so the session settles before the test sends the real
// "/config" command through SendToSession; the /config turn calls config_edit
// with the full replacement document; after the verdict lands it answers once.
// A second call would mean the suspended call was re-executed.
func configEditRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if hasToolResultFor(msgs, tool.IDConfigEdit) {
		return &llmwire.Response{Text: "configuration replaced"}
	}

	if hasUserContaining(msgs, configapply.ConfigEditCommand) {
		return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
			ID:        configEditCallID,
			Name:      tool.IDConfigEdit,
			Arguments: json.RawMessage(`{"document":` + mustQuoteJSON(configEditCandidate) + `}`),
		}}}
	}

	return &llmwire.Response{Text: "ready to reconfigure"}
}

// unauthorizedConfigEditRespond calls config_edit with no /config turn, so the
// session must answer the authorization refusal in-process.
func unauthorizedConfigEditRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if hasToolResultFor(msgs, tool.IDConfigEdit) {
		return &llmwire.Response{Text: "configuration replaced"}
	}

	return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
		ID:        configEditCallID,
		Name:      tool.IDConfigEdit,
		Arguments: json.RawMessage(`{"document":` + mustQuoteJSON(configEditCandidate) + `}`),
	}}}
}

func mustQuoteJSON(s string) string {
	encoded, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}

	return string(encoded)
}

func TestScenario_ConfigQuestionCannotReplaceDocumentWithSandboxFragment(t *testing.T) {
	configDir := newApplyConfigDir(t)
	configPath := filepath.Join(configDir, "config.yaml")
	before, err := os.ReadFile(configPath)
	require.NoError(t, err)
	d := newApplyDaemonWith(t, filepath.Join(t.TempDir(), "config-question.db"), configDir,
		func(_ string, messages []llmwire.Message) *llmwire.Response {
			if hasToolResultFor(messages, tool.IDConfigEdit) {
				return &llmwire.Response{Text: "configuration unchanged"}
			}
			if hasUserContaining(messages, "/config") {
				return &llmwire.Response{ToolCalls: []llmwire.ToolCall{
					{
						ID:   "fragment",
						Name: tool.IDConfigEdit,
						Arguments: json.RawMessage(
							`{"document":"sandbox:\n  enabled: true\n  projects:\n    /tmp/project:\n      rules: []\n"}`,
						),
					},
				}}
			}
			return &llmwire.Response{Text: "ready"}
		})
	defer d.shutdown()
	d.startInboxWake()
	id, err := d.mgr.Send(d.ctx, d.projectID, "hello", "fake-model", map[string]any{"manager_id": "telegram:main"})
	require.NoError(t, err)
	d.waitUntil("opening turn settled", func() bool { return !d.mgr.HasActiveLoop(id) })
	require.NoError(t, d.mgr.SendToSession(d.ctx, id, "/config - is the sandbox unconfigured?"))
	d.waitUntil("invalid replacement answered", func() bool {
		return hasToolResultFor(d.parentMessages(id), tool.IDConfigEdit) && !d.mgr.HasActiveLoop(id)
	})
	after, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Zero(t, d.restartCount())
	pending, err := d.ops.LoadPending()
	require.NoError(t, err)
	assert.Nil(t, pending)
	assert.Contains(t, lastToolResultContent(d.parentMessages(id), tool.IDConfigEdit), "configuration fragments")
}

// startConfigEditSession settles the opener turn, then sends the real /config
// command through the durable input boundary — the same path a manager message
// takes — so the promotion transaction that carries the grant cannot race the
// session's first model call.
func startConfigEditSession(t *testing.T, d *applyDaemon, prompt string) int64 {
	t.Helper()

	d.startInboxWake()
	sessionID, err := d.mgr.Send(
		d.ctx,
		d.projectID,
		prompt,
		"fake-model",
		map[string]any{"manager_id": "telegram:main"},
	)
	require.NoError(t, err)

	d.waitUntil("opener turn settled", func() bool {
		return !d.mgr.HasActiveLoop(sessionID)
	})
	d.mgr.waitIdle(sessionID)

	require.NoError(t, d.mgr.SendToSession(d.ctx, sessionID, configapply.ConfigEditCommand))

	return sessionID
}

// currentActivationOf returns the session's current grant. A missing grant is
// nil; any read failure fails the test instead of passing as absence.
func currentActivationOf(t *testing.T, h *subagentHarness, sessionID int64) *sessionstore.ToolActivation {
	t.Helper()

	activation, err := h.sessStore.
		CurrentActivation(context.Background(), sessionID)
	if errors.Is(err, sessionstore.ErrActivationNotFound) {
		return nil
	}

	require.NoError(t, err)

	return activation
}

// The full happy path: a real /config user turn authorizes exactly one solo
// config_edit call; the apply commits through the marker protocol; the grant is
// spent; the verdict reaches the call only after the restart.
func TestScenario_ConfigEditVerdictReachesTheSessionAfterRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDirWith(t, toolConfig)

	first := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)

	sessionID := startConfigEditSession(t, first, "reconfigure the daemon")

	first.waitForRestart(t)
	first.waitUntil("session suspended on the config_edit call", func() bool {
		return !first.mgr.HasActiveLoop(sessionID)
	})

	pending, err := first.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending, "the commit leaves a marker naming the waiting session")
	require.Equal(t, sessionID, pending.SessionID)
	require.Equal(t, configEditCallID, pending.ToolCallID)
	require.Equal(t, tool.IDConfigEdit, pending.ToolName)

	msgs := first.parentMessages(sessionID)
	require.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit))
	require.Zero(t, countToolResultsFor(msgs, tool.IDConfigEdit), "the call is out with the world")

	consumed := currentActivationOf(t, first.subagentHarness, sessionID)
	require.NotNil(t, consumed, "the /config grant exists")
	require.Equal(t, sessionstore.ActivationConsumed, consumed.State,
		"the successful apply spends the grant; it may not stay pending")

	first.shutdown()

	second := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
	defer second.shutdown()

	require.NoError(t, second.mgr.Start(second.ctx))

	outcome, err := second.bootVerdict(t)
	require.NoError(t, err)
	require.True(t, outcome.Verdict.Applied, outcome.Verdict.Reason())
	assert.False(t, outcome.RolledBack)

	second.waitUntil("config verdict answered", func() bool {
		return countToolResultsFor(second.parentMessages(sessionID), tool.IDConfigEdit) == 1 &&
			!second.mgr.HasActiveLoop(sessionID)
	})
	second.mgr.waitIdle(sessionID)

	msgs = second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit),
		"the suspended call is answered, never re-executed")
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDConfigEdit))
	assert.Contains(t, lastToolResultContent(msgs, tool.IDConfigEdit), "Config applied")
	assert.Equal(t, 0, second.restartCount())

	assert.NoFileExists(t, filepath.Join(configDir, coagenthome.PendingApplyFileName))

	body, err := os.ReadFile(filepath.Join(configDir, "config.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(body), "id: claude-opus-5")
	assert.Contains(t, string(body), "${WORK_API_KEY}", "credentials stay references on disk")
}

// Without a user /config turn the model sees config_edit but gets an
// authorization error before anything is staged.
func TestScenario_ConfigEditWithoutActivationNeverStages(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDirWith(t, toolConfig)

	d := newApplyDaemonWith(t, dbPath, configDir, unauthorizedConfigEditRespond)
	defer d.shutdown()

	require.NoError(t, d.mgr.Start(d.ctx))

	// The responder calls config_edit; no activation exists, so the session must
	// be answered with the authorization refusal, in-process.
	d.startInboxWake()
	sessionID, err := d.mgr.Send(
		d.ctx,
		d.projectID,
		"reconfigure the daemon",
		"fake-model",
		map[string]any{"manager_id": "telegram:main"},
	)
	require.NoError(t, err)

	d.waitUntil("the refused call reached the transcript", func() bool {
		return countToolResultsFor(d.parentMessages(sessionID), tool.IDConfigEdit) == 1
	})
	d.mgr.waitIdle(sessionID)

	msgs := d.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit))
	assert.Contains(t, lastToolResultContent(msgs, tool.IDConfigEdit), "/config")
	assert.Zero(t, d.restartCount(), "an unauthorized call never applies")
	assert.Equal(t, toolConfig, configBytesOf(t, configDir))
	assert.Nil(t, currentActivationOf(t, d.subagentHarness, sessionID))
}

// A syntactically valid candidate whose boot fails is rolled back and the
// rejection reaches the session — the same ResolvePending contract main.go runs.
func TestScenario_ConfigEditBootInvalidCandidateRollsBack(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDirWith(t, toolConfig)

	first := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)

	sessionID := startConfigEditSession(t, first, "reconfigure the daemon")

	first.waitForRestart(t)
	first.waitUntil("session suspended on the config_edit call", func() bool {
		return !first.mgr.HasActiveLoop(sessionID)
	})

	pending, err := first.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending, "a staged candidate is committed before the restart")

	first.shutdown()

	second := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
	defer second.shutdown()

	// The second image comes up on a config it cannot serve: ResolvePending is
	// handed the boot error the real daemon would carry, and must roll back.
	outcome, err := second.ops.ResolvePending(*pending, errors.New("cold catalog: unknown model claude-opus-9"))
	require.NoError(t, err)
	require.True(t, outcome.Verdict.Failed())
	assert.True(t, outcome.RolledBack)

	second.startInboxWake()
	message := "Config change rejected — " + outcome.Verdict.Reason()
	_, err = enqueueCallResult(

		second.ctx, second.mgr.store,

		outcome.Pending.SessionID,
		outcome.Pending.ToolCallID,
		outcome.Pending.ToolName,
		message,
	)
	require.NoError(t, err)
	require.NoError(t, second.ops.ClearPending(outcome.Pending))

	second.waitUntil("config verdict answered", func() bool {
		return countToolResultsFor(second.parentMessages(sessionID), tool.IDConfigEdit) == 1 &&
			!second.mgr.HasActiveLoop(sessionID)
	})
	second.mgr.waitIdle(sessionID)

	msgs := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDConfigEdit))
	assert.Contains(t, lastToolResultContent(msgs, tool.IDConfigEdit), "rolled back")

	assert.Equal(t, toolConfig, configBytesOf(t, configDir), "the rollback restores the previous config")
}

// The verdict is delivered exactly once even when the first image died between
// the write and the delivery: a replayed delivery inserts no second result.
func TestScenario_ConfigEditVerdictRedeliveryIsIdempotent(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDirWith(t, toolConfig)

	first := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)

	sessionID := startConfigEditSession(t, first, "reconfigure the daemon")

	first.waitForRestart(t)
	first.waitUntil("session suspended on the config_edit call", func() bool {
		return !first.mgr.HasActiveLoop(sessionID)
	})

	first.shutdown()

	second := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)

	pending, err := second.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)

	_, err = second.ops.ResolvePending(*pending, nil)
	require.NoError(t, err)

	applied, err := enqueueCallResult(

		second.ctx, second.mgr.store,

		sessionID,
		configEditCallID,
		tool.IDConfigEdit,
		"Config applied: replace configuration document",
	)
	require.NoError(t, err)
	require.True(t, applied)

	// Dies after the injection, before the acknowledgement.
	second.shutdown()

	third := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
	defer third.shutdown()

	require.NoError(t, third.mgr.Start(third.ctx))

	replay, err := third.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, replay, "an unacknowledged verdict is replayed")

	_, err = third.ops.ResolvePending(*replay, nil)
	require.NoError(t, err)

	applied, err = enqueueCallResult(

		third.ctx, third.mgr.store,

		sessionID,
		configEditCallID,
		tool.IDConfigEdit,
		"Config applied: replace configuration document",
	)
	require.NoError(t, err)
	assert.False(t, applied, "a replayed verdict for the same call inserts nothing")

	require.NoError(t, third.ops.ClearPending(*replay))

	third.waitUntil("config verdict answered", func() bool {
		return countToolResultsFor(third.parentMessages(sessionID), tool.IDConfigEdit) == 1 &&
			!third.mgr.HasActiveLoop(sessionID)
	})
	third.mgr.waitIdle(sessionID)

	msgs := third.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDConfigEdit))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit))
}

func configBytesOf(t *testing.T, configDir string) string {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(configDir, "config.yaml"))
	require.NoError(t, err)

	return string(body)
}
