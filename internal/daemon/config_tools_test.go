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

	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

// toolConfig is a valid starting point every config-tool test mutates from.
const toolConfig = `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
models:
    - id: claude-sonnet-5
      provider: work
    - id: claude-opus-5
      provider: work
`

//nolint:gosec // fake credentials
const toolSecrets = "WORK_API_KEY=sk-ant-work-0000000000\n"

type configHarness struct {
	mgr *svc
	// sessionID is a real session row: the apply pipeline reads its transcript
	// before it commits, so a config tool needs somewhere to have suspended.
	sessionID int64
	projectID int64
	sessions  *sessionstore.Store
	store     Store
	factory   *mockFactory
	tools     map[string]tool.Tool
	restarts  int
	config    string
}

func newConfigHarness(t *testing.T) *configHarness {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	secretsPath := filepath.Join(dir, "secrets")

	require.NoError(t, os.WriteFile(configPath, []byte(toolConfig), 0o600))
	require.NoError(t, os.WriteFile(secretsPath, []byte(toolSecrets), 0o600))

	h := &configHarness{config: configPath}
	mgr, factory, store := newTestManager(t)
	sessions, ok := mgr.store.(*sessionstore.Store)
	require.True(t, ok)
	h.sessions = sessions
	h.store = store
	h.factory = factory
	projectID, err := store.GetOrCreateProject(context.Background(), t.TempDir())
	require.NoError(t, err)
	h.projectID = projectID
	mgr.applier = configapply.New(configops.New(configPath, secretsPath), h.sessions)

	h.mgr = mgr
	h.sessionID = h.liveSession(t)
	h.tools = map[string]tool.Tool{tool.IDConfigEdit: configapply.NewConfigEdit(h.sessionID, mgr.applier)}

	return h
}

// configHarnessCandidate reorders the models, so staging it visibly changes the file.
const configHarnessCandidate = `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
models:
    - id: claude-opus-5
      provider: work
    - id: claude-sonnet-5
      provider: work
`

// grantedCall carries what config_edit demands: the call id plus the durable
// /config grant the tool revalidates before staging.
func grantedCall(ctx context.Context, sessionID int64, callID string) context.Context {
	ctx = tool.WithCallID(ctx, callID)

	return tool.WithActivationGrant(ctx, tool.ActivationGrant{
		SessionID: sessionID, ToolID: tool.IDConfigEdit, Command: "/config", ToolCallID: callID,
	})
}

func configEditArgs(document string) json.RawMessage {
	encoded, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}

	return json.RawMessage(`{"document":` + string(encoded) + `}`)
}

// grantedCall runs config_edit the way the loop does: the assistant turn is
// persisted first, then the tool executes with the grant in context.
func (h *configHarness) grantedCall(t *testing.T, callID, document string) error {
	t.Helper()

	h.recordCall(t, callID, tool.IDConfigEdit)

	_, err := h.tools[tool.IDConfigEdit].Execute(
		grantedCall(context.Background(), h.sessionID, callID), configEditArgs(document),
	)
	if errors.Is(err, tool.ErrSuspend) {
		require.NoError(
			t,
			h.sessions.UpdateSessionStatus(t.Context(), h.sessionID, sessionstore.SessionStatusSuspended),
		)
	}

	return err
}

// liveSession creates a real session record, so notification delivery has
// somewhere to land.
func (h *configHarness) liveSession(t *testing.T) int64 {
	t.Helper()

	ctx := context.Background()
	rec, err := h.mgr.store.CreateSession(
		ctx, h.projectID, "fake-model", "", map[string]any{"channel": "cli", "manager_id": "cli"},
	)
	require.NoError(t, err)

	return rec.ID
}

// recordCall appends the assistant turn a tool_call arrives in, which is what
// makes a later suspend durable.
func (h *configHarness) recordCall(t *testing.T, callID, toolName string) {
	t.Helper()
	activation, activationErr := h.sessions.PendingActivation(t.Context(), h.sessionID)
	if activationErr == nil && !h.mgr.applier.Has(h.sessionID) {
		_, err := h.sessions.Commit(t.Context(), sessionstore.Commit{
			SessionID: h.sessionID,
			Activation: &sessionstore.ActivationChange{
				InputID: activation.InputID,
				State:   sessionstore.ActivationExpired,
			},
		})
		require.NoError(t, err)
	}

	if !h.mgr.applier.Has(h.sessionID) {
		input, err := h.sessions.Enqueue(
			context.Background(),
			sessionstore.Input{SessionID: h.sessionID, Source: sessionstore.InputSourceUser, Content: "/config"},
		)
		require.NoError(t, err)
		_, err = h.sessions.Commit(
			context.Background(),
			sessionstore.Commit{
				SessionID: h.sessionID,
				Accept: []sessionstore.Accept{
					{
						InputID:    input.Input.ID,
						State:      sessionstore.InputStateAccepted,
						Content:    "/config",
						LinkRef:    -1,
						ModelBound: true,
					},
				},
				Activation: &sessionstore.ActivationChange{
					InputID: input.Input.ID,
					State:   sessionstore.ActivationPending,
					ToolID:  toolName,
					Command: "/config",
				},
			},
		)
		require.NoError(t, err)
	}
	calls, err := json.Marshal([]llmwire.ToolCall{{ID: callID, Name: toolName}})
	require.NoError(t, err)

	_, err = h.sessions.Commit(
		context.Background(),
		sessionstore.Commit{SessionID: h.sessionID, Messages: []*transcript.Message{{
			Role:      llmwire.RoleAssistant,
			ToolCalls: calls,
		}}},
	)
	require.NoError(t, err)
}

// restart models the boot a committed apply causes: the new process image comes
// up with a free apply slot and delivers the verdict the call was waiting for.
func (h *configHarness) restart(t *testing.T, callID, toolName string) {
	t.Helper()

	h.restarts += len(h.mgr.applier.Restart())
	h.mgr.applier.ReleaseApply()
	h.mgr.applier = configapply.New(h.mgr.applier.Ops(), h.sessions)
	h.tools[tool.IDConfigEdit] = configapply.NewConfigEdit(h.sessionID, h.mgr.applier)

	_, err := h.sessions.Commit(
		context.Background(),
		sessionstore.Commit{SessionID: h.sessionID, Messages: []*transcript.Message{{
			Role:       llmwire.RoleTool,
			ToolCallID: callID,
			ToolName:   toolName,
			Content:    "Config applied.",
		}}},
	)
	require.NoError(t, err)
}

func (h *configHarness) restartCount() int { return h.restarts + len(h.mgr.applier.Restart()) }

func (h *configHarness) configBytes(t *testing.T) string {
	t.Helper()

	data, err := os.ReadFile(h.config)
	require.NoError(t, err)

	return string(data)
}

// A staged change suspends: nothing is written until the loop has persisted the
// suspend and the daemon runs the apply.
func TestConfigTool_SuccessStagesAndSuspends(t *testing.T) {
	h := newConfigHarness(t)

	require.ErrorIs(t, h.grantedCall(t, "c1", configHarnessCandidate), tool.ErrSuspend)

	assert.Equal(t, toolConfig, h.configBytes(t), "nothing is written by the tool itself")
	assert.True(t, h.mgr.applier.Has(h.sessionID))
	assert.Equal(t, map[string]string{"c1": tool.IDConfigEdit}, h.mgr.applier.Calls(h.sessionID))
	assert.Equal(t, 0, h.restartCount())

	h.mgr.applier.RunStagedApply(context.Background(), h.sessionID)

	assert.Equal(t, 1, h.restartCount(), "the apply asks the daemon to come back")
	assert.Contains(t, h.configBytes(t), "id: claude-opus-5\n      provider: work\n    - id: claude-sonnet-5")
	assert.True(t, h.mgr.applier.Has(h.sessionID), "the call stays open until its verdict arrives")
}

// Guard violations are ordinary tool errors: nothing staged, no suspend, no
// restart — the model can correct itself in the same turn.
func TestConfigTool_GuardViolationsAreImmediateErrors(t *testing.T) {
	h := newConfigHarness(t)

	t.Run("missing activation grant", func(t *testing.T) {
		h.recordCall(t, "c-grant", tool.IDConfigEdit)

		_, err := h.tools[tool.IDConfigEdit].Execute(
			tool.WithCallID(context.Background(), "c-grant"), configEditArgs(configHarnessCandidate),
		)
		require.Error(t, err)
		require.NotErrorIs(t, err, tool.ErrSuspend)
		assert.Contains(t, err.Error(), "/config")
	})

	t.Run("invalid document", func(t *testing.T) {
		err := h.grantedCall(t, "c-var", "providers: [unclosed\n")
		require.Error(t, err)
		require.NotErrorIs(t, err, tool.ErrSuspend, "a refusal must not suspend the session")
	})

	t.Run("empty document", func(t *testing.T) {
		err := h.grantedCall(t, "c-doc", "")
		require.Error(t, err)
		require.NotErrorIs(t, err, tool.ErrSuspend)
		assert.Contains(t, err.Error(), "document is required")
	})

	assert.False(t, h.mgr.applier.Has(h.sessionID))
	assert.Equal(t, 0, h.restartCount())
	assert.Equal(t, toolConfig, h.configBytes(t))
}

// The apply pipeline hands over a staged change exactly once, so a second run —
// after a verdict, or after a wake for some other reason — cannot repeat it.
func TestRunStagedApply_HandsOverExactlyOnce(t *testing.T) {
	h := newConfigHarness(t)

	require.ErrorIs(t, h.grantedCall(t, "c1", configHarnessCandidate), tool.ErrSuspend)

	h.mgr.applier.RunStagedApply(context.Background(), h.sessionID)
	h.mgr.applier.RunStagedApply(context.Background(), h.sessionID)

	assert.Equal(t, 1, h.restartCount(), "the second pass finds nothing to apply")
}

// Two applies in sequence: the first is answered by its verdict, and only then
// does the session get to make another.
func TestConfigTool_TwoAppliesInSequence(t *testing.T) {
	ctx := context.Background()
	h := newConfigHarness(t)

	require.ErrorIs(t, h.grantedCall(t, "c1", configHarnessCandidate), tool.ErrSuspend)
	h.mgr.applier.RunStagedApply(ctx, h.sessionID)

	// The daemon comes back and delivers the verdict.
	h.restart(t, "c1", tool.IDConfigEdit)
	assert.False(t, h.mgr.applier.Has(h.sessionID))

	require.ErrorIs(t, h.grantedCall(t, "c2", toolConfig), tool.ErrSuspend)
	h.mgr.applier.RunStagedApply(ctx, h.sessionID)

	assert.Equal(t, 2, h.restartCount())
	assert.Contains(t, h.configBytes(t), "id: claude-sonnet-5\n      provider: work\n    - id: claude-opus-5")
}

// A call with no tool_call id has nothing to answer against; suspending would
// strand the session.
func TestConfigTool_RefusesWithoutACallID(t *testing.T) {
	h := newConfigHarness(t)

	_, err := h.tools[tool.IDConfigEdit].Execute(
		tool.WithActivationGrant(context.Background(), tool.ActivationGrant{
			SessionID: h.sessionID, ToolID: tool.IDConfigEdit, Command: "/config",
		}),
		configEditArgs(configHarnessCandidate),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tool_call id")
}

// config_edit lives on every root session with an applier, never on children.
// The tool description states the restart contract a caller cannot see.
func TestConfigTool_DescriptionsCarryTheContract(t *testing.T) {
	h := newConfigHarness(t)

	assert.Contains(t, h.tools[tool.IDConfigEdit].Description(), "restarts the daemon")
}

// Two config changes in one turn: the second is refused outright. An apply ends
// in a restart, so only the change staged against the config the daemon comes
// back on can be trusted — and a silently dropped second one would strand the
// call that made it.
func TestConfigTool_RefusesASecondApplyInTheSameTurn(t *testing.T) {
	h := newConfigHarness(t)

	require.ErrorIs(t, h.grantedCall(t, "c1", configHarnessCandidate), tool.ErrSuspend)

	err := h.grantedCall(t, "c2", toolConfig)
	require.Error(t, err)
	require.NotErrorIs(t, err, tool.ErrSuspend, "a refused stage must not suspend a second call")
	assert.Contains(t, err.Error(), "one change at a time")

	assert.Equal(t, map[string]string{"c1": tool.IDConfigEdit}, h.mgr.applier.Calls(h.sessionID))

	h.mgr.applier.RunStagedApply(context.Background(), h.sessionID)
	assert.Equal(t, 1, h.restartCount())
}

// The apply slot is one, daemon-wide. The marker, the config file and the
// restart an apply ends in are all global, so a second staged change — from any
// session — would overwrite the first and strand the call it belongs to.
// The whole document replaces the config: a literal credential and an extra
// model land in the file exactly as written.
func TestConfigTool_WholeDocumentReachesTheConfig(t *testing.T) {
	h := newConfigHarness(t)

	document := toolConfig + "    - id: claude-haiku-4-5\n      provider: work\n"
	require.ErrorIs(t, h.grantedCall(t, "c1", document), tool.ErrSuspend)
	h.mgr.applier.RunStagedApply(context.Background(), h.sessionID)

	assert.Contains(t, h.configBytes(t), "id: claude-haiku-4-5")
}

func TestConfigTool_DeliversOneVerdictAfterRestart(t *testing.T) {
	h := newConfigHarness(t)

	require.ErrorIs(t, h.grantedCall(t, "tags-1", configHarnessCandidate), tool.ErrSuspend)
	h.mgr.applier.RunStagedApply(context.Background(), h.sessionID)
	h.restart(t, "tags-1", tool.IDConfigEdit)

	assert.False(t, h.mgr.applier.Has(h.sessionID))
	assert.Equal(t, 1, h.restartCount())
	messages, err := h.sessions.LoadActiveMessages(context.Background(), h.sessionID)
	require.NoError(t, err)
	var verdicts int
	for _, message := range messages {
		if message.ToolName == tool.IDConfigEdit && message.Content == "Config applied." {
			verdicts++
		}
	}
	assert.Equal(t, 1, verdicts)
}
