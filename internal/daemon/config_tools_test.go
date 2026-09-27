package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionevent"
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
	sessions  sessionstore.Store
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
	sessions, ok := mgr.sessionStore.(sessionstore.Store)
	require.True(t, ok)
	h.sessions = sessions
	h.store = store
	h.factory = factory
	projectID, err := store.GetOrCreateProject(context.Background(), t.TempDir())
	require.NoError(t, err)
	h.projectID = projectID
	mgr.applier = configapply.New(configops.New(configPath, secretsPath), func() { h.restarts++ })

	h.mgr = mgr
	h.sessionID = h.liveSession(t)
	h.tools = map[string]tool.Tool{tool.IDConfigEdit: newConfigEditTool(mgr, h.sessionID)}

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

	return err
}

const testSessionID = 42

// liveSession creates a real session record, so notification delivery has
// somewhere to land.
func (h *configHarness) liveSession(t *testing.T) int64 {
	t.Helper()

	ctx := context.Background()
	rec, err := h.mgr.sessionStore.CreateSession(
		ctx, h.projectID, "fake-model", "", map[string]any{"channel": "cli"},
	)
	require.NoError(t, err)

	return rec.ID
}

// recordCall appends the assistant turn a tool_call arrives in, which is what
// makes a later suspend durable.
func (h *configHarness) recordCall(t *testing.T, callID, toolName string) {
	t.Helper()

	calls, err := json.Marshal([]llmwire.ToolCall{{ID: callID, Name: toolName}})
	require.NoError(t, err)

	_, err = h.sessions.InsertMessage(context.Background(), h.sessionID, &transcript.Message{
		Role:      llmwire.RoleAssistant,
		ToolCalls: calls,
	})
	require.NoError(t, err)
}

// restart models the boot a committed apply causes: the new process image comes
// up with a free apply slot and delivers the verdict the call was waiting for.
func (h *configHarness) restart(t *testing.T, callID, toolName string) {
	t.Helper()

	h.mgr.applier.ReleaseApply()
	h.mgr.staged.resolve(h.sessionID, callID)

	_, err := h.sessions.InsertMessage(context.Background(), h.sessionID, &transcript.Message{
		Role:       llmwire.RoleTool,
		ToolCallID: callID,
		ToolName:   toolName,
		Content:    "Config applied.",
	})
	require.NoError(t, err)
}

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
	assert.True(t, h.mgr.staged.has(h.sessionID))
	assert.Equal(t, map[string]string{"c1": tool.IDConfigEdit}, h.mgr.staged.forSession(h.sessionID))
	assert.Equal(t, 0, h.restarts)

	h.mgr.runStagedApply(context.Background(), h.sessionID)

	assert.Equal(t, 1, h.restarts, "the apply asks the daemon to come back")
	assert.Contains(t, h.configBytes(t), "id: claude-opus-5\n      provider: work\n    - id: claude-sonnet-5")
	assert.True(t, h.mgr.staged.has(h.sessionID), "the call stays open until its verdict arrives")
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

	assert.False(t, h.mgr.staged.has(h.sessionID))
	assert.Equal(t, 0, h.restarts)
	assert.Equal(t, toolConfig, h.configBytes(t))
}

// The apply pipeline hands over a staged change exactly once, so a second run —
// after a verdict, or after a wake for some other reason — cannot repeat it.
func TestRunStagedApply_HandsOverExactlyOnce(t *testing.T) {
	h := newConfigHarness(t)

	require.ErrorIs(t, h.grantedCall(t, "c1", configHarnessCandidate), tool.ErrSuspend)

	h.mgr.runStagedApply(context.Background(), h.sessionID)
	h.mgr.runStagedApply(context.Background(), h.sessionID)

	assert.Equal(t, 1, h.restarts, "the second pass finds nothing to apply")
}

// Two applies in sequence: the first is answered by its verdict, and only then
// does the session get to make another.
func TestConfigTool_TwoAppliesInSequence(t *testing.T) {
	ctx := context.Background()
	h := newConfigHarness(t)

	require.ErrorIs(t, h.grantedCall(t, "c1", configHarnessCandidate), tool.ErrSuspend)
	h.mgr.runStagedApply(ctx, h.sessionID)

	// The daemon comes back and delivers the verdict.
	h.restart(t, "c1", tool.IDConfigEdit)
	assert.False(t, h.mgr.staged.has(h.sessionID))

	require.ErrorIs(t, h.grantedCall(t, "c2", toolConfig), tool.ErrSuspend)
	h.mgr.runStagedApply(ctx, h.sessionID)

	assert.Equal(t, 2, h.restarts)
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
func TestRegisterConfigEditTool_RootsOnly(t *testing.T) {
	ctx := context.Background()
	h := newConfigHarness(t)

	tests := []struct {
		name string
		rec  *sessionstore.SessionRecord
		want bool
	}{
		{
			name: "ordinary root",
			rec:  &sessionstore.SessionRecord{ID: testSessionID, ProjectID: h.projectID},
			want: true,
		},
		{
			name: "manager-owned root",
			rec: &sessionstore.SessionRecord{
				ID: testSessionID, ProjectID: h.projectID,
				Attributes: map[string]any{
					controllerapi.SessionAttributeManagerID: "telegram-main",
				},
			},
			want: true,
		},
		{
			name: "child",
			rec: &sessionstore.SessionRecord{
				ID: 43, ProjectID: h.projectID, ParentID: testSessionID,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := &mockSession{}
			h.mgr.registerConfigEditTool(ctx, tt.rec, sess)
			assert.Equal(t, tt.want, sess.hasTool(tool.IDConfigEdit))
		})
	}
}

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

	assert.Equal(t, map[string]string{"c1": tool.IDConfigEdit}, h.mgr.staged.forSession(h.sessionID))

	h.mgr.runStagedApply(context.Background(), h.sessionID)
	assert.Equal(t, 1, h.restarts)
}

// The apply slot is one, daemon-wide. The marker, the config file and the
// restart an apply ends in are all global, so a second staged change — from any
// session — would overwrite the first and strand the call it belongs to.
func TestStagedCalls_ApplySlotIsDaemonWide(t *testing.T) {
	h := newConfigHarness(t)

	assert.True(t, h.mgr.stageApply(1, "a", tool.IDConfigEdit, &configops.Staged{}))
	assert.False(t, h.mgr.stageApply(1, "b", tool.IDConfigEdit, &configops.Staged{}))
	assert.False(t, h.mgr.stageApply(2, "a", tool.IDConfigEdit, &configops.Staged{}),
		"another session writes the same config file")

	_, _, ok := h.mgr.staged.takePendingApply(1)
	require.True(t, ok)

	assert.False(t, h.mgr.stageApply(1, "b", tool.IDConfigEdit, &configops.Staged{}),
		"handing the change to the pipeline does not free the slot — only a commit that failed does")

	h.mgr.applier.ReleaseApply()
	assert.True(t, h.mgr.stageApply(1, "b", tool.IDConfigEdit, &configops.Staged{}))
}

// panicSession is a session whose loop dies the way a bug in it would: the
// runner's recovery keeps the daemon alive, so whatever the loop was holding is
// held for the rest of the process image.
type panicSession struct{ *mockSession }

func (p *panicSession) RunDaemon(
	context.Context,
	func(sessionevent.Notification),
	func(bool),
) (session.RunResult, error) {
	panic("session loop bug")
}

// The apply slot is process-global and is given back by exactly two things: a
// commit that failed, or the restart a commit that landed causes. A loop that
// dies in between does neither — so the runner teardown has to, or no session
// can ever change the config again on this image.
func TestRunSession_ALoopThatDiesAfterClaimingGivesTheApplySlotBack(t *testing.T) {
	ctx := context.Background()
	h := newConfigHarness(t)

	defer h.mgr.Shutdown(5 * time.Second)

	sessionID := h.liveSession(t)
	tools := map[string]tool.Tool{tool.IDConfigEdit: newConfigEditTool(h.mgr, sessionID)}

	_, err := tools[tool.IDConfigEdit].Execute(
		grantedCall(ctx, sessionID, "c1"), configEditArgs(configHarnessCandidate),
	)
	require.ErrorIs(t, err, tool.ErrSuspend)

	h.factory.nextSess = &panicSession{mockSession: &mockSession{}}
	require.NoError(t, h.mgr.SendToSession(ctx, sessionID, "carry on"))

	require.Eventually(t, func() bool {
		return !h.mgr.staged.has(sessionID)
	}, 5*time.Second, 10*time.Millisecond, "the call the dead loop owed is never answered")

	assert.True(t, h.mgr.stageApply(sessionID, "c2", tool.IDConfigEdit, &configops.Staged{}),
		"the apply slot was never given back")
	assert.Equal(t, toolConfig, h.configBytes(t), "a change that died before the commit writes nothing")
}

// The whole document replaces the config: a literal credential and an extra
// model land in the file exactly as written.
func TestConfigTool_WholeDocumentReachesTheConfig(t *testing.T) {
	h := newConfigHarness(t)

	document := toolConfig + "    - id: claude-haiku-4-5\n      provider: work\n"
	require.ErrorIs(t, h.grantedCall(t, "c1", document), tool.ErrSuspend)
	h.mgr.runStagedApply(context.Background(), h.sessionID)

	assert.Contains(t, h.configBytes(t), "id: claude-haiku-4-5")
}

func TestConfigTool_DeliversOneVerdictAfterRestart(t *testing.T) {
	h := newConfigHarness(t)

	require.ErrorIs(t, h.grantedCall(t, "tags-1", configHarnessCandidate), tool.ErrSuspend)
	h.mgr.runStagedApply(context.Background(), h.sessionID)
	h.restart(t, "tags-1", tool.IDConfigEdit)

	assert.False(t, h.mgr.staged.has(h.sessionID))
	assert.Equal(t, 1, h.restarts)
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
