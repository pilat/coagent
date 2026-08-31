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
	systemWorkDir := filepath.Join(t.TempDir(), controllerapi.CoagentSystemProjectDir)
	projectID, err := store.GetOrCreateSystemProject(
		context.Background(),
		systemWorkDir,
		controllerapi.CoagentSystemProjectName,
	)
	require.NoError(t, err)
	h.projectID = projectID
	mgr.systemProject = systemWorkDir
	mgr.applier = configapply.New(configops.New(configPath, secretsPath), func() { h.restarts++ })

	h.mgr = mgr
	h.sessionID = h.liveSession(t)
	h.tools = make(map[string]tool.Tool)

	for _, tl := range newConfigTools(mgr, h.sessionID) {
		h.tools[tl.ID()] = tl
	}

	return h
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

// call runs a tool the way the loop does: the assistant turn carrying the
// tool_call is persisted first, then the tool executes with that id in context.
func (h *configHarness) call(t *testing.T, id, callID, params string) error {
	t.Helper()

	h.recordCall(t, callID, id)

	ctx := tool.WithCallID(context.Background(), callID)

	_, err := h.tools[id].Execute(ctx, json.RawMessage(params))

	return err
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

	err := h.call(t, tool.IDSetDefaultModel, "c1", `{"id":"claude-opus-5"}`)
	require.ErrorIs(t, err, tool.ErrSuspend)

	assert.Equal(t, toolConfig, h.configBytes(t), "nothing is written by the tool itself")
	assert.True(t, h.mgr.staged.has(h.sessionID))
	assert.Equal(t, map[string]string{"c1": tool.IDSetDefaultModel}, h.mgr.staged.forSession(h.sessionID))
	assert.Equal(t, 0, h.restarts)

	h.mgr.runStagedApply(context.Background(), h.sessionID)

	assert.Equal(t, 1, h.restarts, "the apply asks the daemon to come back")
	assert.Contains(t, h.configBytes(t), "id: claude-opus-5\n      provider: work\n    - id: claude-sonnet-5")
	assert.True(t, h.mgr.staged.has(h.sessionID), "the call stays open until its verdict arrives")
}

// Guard violations are ordinary tool errors: nothing staged, no suspend, no
// restart — the model can correct itself in the same turn.
func TestConfigTool_GuardViolationsAreImmediateErrors(t *testing.T) {
	tests := []struct {
		name   string
		id     string
		params string
		want   string
	}{
		{
			name:   "removing the only provider",
			id:     tool.IDRemoveProvider,
			params: `{"name":"work"}`,
			want:   "only provider",
		},
		{
			name:   "removing the default model without a replacement",
			id:     tool.IDRemoveModel,
			params: `{"id":"claude-sonnet-5"}`,
			want:   "name its replacement",
		},
		{
			name:   "a literal credential",
			id:     tool.IDSetProvider,
			params: `{"name":"second","driver":"anthropic","api_key":"sk-ant-literal-value"}`,
			want:   "${VAR} reference",
		},
		{
			name:   "a model whose provider does not exist",
			id:     tool.IDAddModel,
			params: `{"id":"x","provider":"ghost"}`,
			want:   `no provider named "ghost"`,
		},
		{
			name:   "an unknown default",
			id:     tool.IDSetDefaultModel,
			params: `{"id":"nope"}`,
			want:   `no model named "nope"`,
		},
		{
			name:   "a manager with no token to reference",
			id:     tool.IDSetManager,
			params: `{"id":"tg","driver":"telegram"}`,
			want:   "bot_token reference",
		},
		{
			name:   "ambiguous manager parameters",
			id:     tool.IDSetManager,
			params: `{"id":"tg","id":"tg2"}`,
			want:   "duplicate key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newConfigHarness(t)

			err := h.call(t, tt.id, "c1", tt.params)
			require.Error(t, err)
			require.NotErrorIs(t, err, tool.ErrSuspend, "a refusal must not suspend the session")
			assert.Contains(t, err.Error(), tt.want)

			assert.False(t, h.mgr.staged.has(h.sessionID))
			assert.Equal(t, 0, h.restarts)
			assert.Equal(t, toolConfig, h.configBytes(t))
		})
	}
}

// The apply pipeline hands over a staged change exactly once, so a second run —
// after a verdict, or after a wake for some other reason — cannot repeat it.
func TestRunStagedApply_HandsOverExactlyOnce(t *testing.T) {
	h := newConfigHarness(t)

	require.ErrorIs(t, h.call(t, tool.IDSetDefaultModel, "c1", `{"id":"claude-opus-5"}`), tool.ErrSuspend)

	h.mgr.runStagedApply(context.Background(), h.sessionID)
	h.mgr.runStagedApply(context.Background(), h.sessionID)

	assert.Equal(t, 1, h.restarts, "the second pass finds nothing to apply")
}

// Two applies in sequence: the first is answered by its verdict, and only then
// does the session get to make another.
func TestConfigTool_TwoAppliesInSequence(t *testing.T) {
	ctx := context.Background()
	h := newConfigHarness(t)

	require.ErrorIs(t, h.call(t, tool.IDSetDefaultModel, "c1", `{"id":"claude-opus-5"}`), tool.ErrSuspend)
	h.mgr.runStagedApply(ctx, h.sessionID)

	// The daemon comes back and delivers the verdict.
	h.restart(t, "c1", tool.IDSetDefaultModel)
	assert.False(t, h.mgr.staged.has(h.sessionID))

	require.ErrorIs(t, h.call(t, tool.IDSetDefaultModel, "c2", `{"id":"claude-sonnet-5"}`), tool.ErrSuspend)
	h.mgr.runStagedApply(ctx, h.sessionID)

	assert.Equal(t, 2, h.restarts)
	assert.Contains(t, h.configBytes(t), "id: claude-sonnet-5\n      provider: work\n    - id: claude-opus-5")
}

// A call with no tool_call id has nothing to answer against; suspending would
// strand the session.
func TestConfigTool_RefusesWithoutACallID(t *testing.T) {
	h := newConfigHarness(t)

	_, err := h.tools[tool.IDSetDefaultModel].Execute(
		context.Background(), json.RawMessage(`{"id":"claude-opus-5"}`),
	)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tool_call id")
}

// Configuration belongs to the reserved system project. Neither a root in an
// ordinary project nor a child may reshape the daemon.
func TestRegisterConfigTools_SystemProjectRootOnly(t *testing.T) {
	ctx := context.Background()
	h := newConfigHarness(t)
	ordinaryProjectID := testProject(t, h.store, t.TempDir())
	rogueProjectID, err := h.store.GetOrCreateSystemProject(
		ctx,
		filepath.Join(t.TempDir(), controllerapi.CoagentSystemProjectDir),
		controllerapi.CoagentSystemProjectName,
	)
	require.NoError(t, err)
	ids := []string{
		tool.IDSetProvider, tool.IDRemoveProvider, tool.IDSetManager, tool.IDRemoveManager,
		tool.IDAddModel, tool.IDRemoveModel, tool.IDSetDefaultModel, tool.IDSetModelTags,
	}
	tests := []struct {
		name string
		rec  *sessionstore.SessionRecord
		want bool
	}{
		{
			name: "configuration root without channel",
			rec: &sessionstore.SessionRecord{
				ID: testSessionID, ProjectID: h.projectID,
			},
			want: true,
		},
		{
			name: "ordinary cli root",
			rec: &sessionstore.SessionRecord{
				ID: testSessionID, ProjectID: ordinaryProjectID,
				Attributes: map[string]any{"channel": "cli"},
			},
		},
		{
			name: "foreign manager in configuration project",
			rec: &sessionstore.SessionRecord{
				ID: testSessionID, ProjectID: h.projectID,
				Attributes: map[string]any{
					controllerapi.SessionAttributeManagerID: "telegram-main",
				},
			},
		},
		{
			name: "system name outside canonical directory",
			rec:  &sessionstore.SessionRecord{ID: testSessionID, ProjectID: rogueProjectID},
		},
		{
			name: "configuration child",
			rec: &sessionstore.SessionRecord{
				ID: 43, ProjectID: h.projectID, ParentID: testSessionID,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sess := &mockSession{}
			h.mgr.registerConfigTools(ctx, tt.rec, sess)

			for _, id := range ids {
				assert.Equal(t, tt.want, sess.hasTool(id), id)
			}
		})
	}
}

func TestRegisterConfigTools_SystemProjectIdentitySurvivesClear(t *testing.T) {
	h := newConfigHarness(t)

	newID, err := h.mgr.Clear(context.Background(), h.sessionID)
	require.NoError(t, err)

	rec, err := h.mgr.sessionStore.GetSession(context.Background(), newID)
	require.NoError(t, err)
	assert.Equal(t, h.projectID, rec.ProjectID)

	sess := &mockSession{}
	h.mgr.registerConfigTools(context.Background(), rec, sess)
	assert.True(t, sess.hasTool(tool.IDSetProvider))
	assert.True(t, sess.hasTool(tool.IDSetDefaultModel))
}

// Every mutating tool has to say what a caller cannot see: the restart, and the
// ${VAR}-only rule wherever a credential is accepted.
func TestConfigTool_DescriptionsCarryTheContract(t *testing.T) {
	h := newConfigHarness(t)

	for id, tl := range h.tools {
		assert.Contains(t, tl.Description(), "restarts the daemon", id)
	}

	for _, id := range []string{tool.IDSetProvider, tool.IDSetManager} {
		assert.Contains(t, string(h.tools[id].Parameters()), "never the value itself", id)
	}
}

// Two config changes in one turn: the second is refused outright. An apply ends
// in a restart, so only the change staged against the config the daemon comes
// back on can be trusted — and a silently dropped second one would strand the
// call that made it.
func TestConfigTool_RefusesASecondApplyInTheSameTurn(t *testing.T) {
	h := newConfigHarness(t)

	require.ErrorIs(t, h.call(t, tool.IDSetDefaultModel, "c1", `{"id":"claude-opus-5"}`), tool.ErrSuspend)

	err := h.call(t, tool.IDAddModel, "c2", `{"id":"claude-haiku-4-5","provider":"work"}`)
	require.Error(t, err)
	require.NotErrorIs(t, err, tool.ErrSuspend, "a refused stage must not suspend a second call")
	assert.Contains(t, err.Error(), "one change at a time")

	assert.Equal(t, map[string]string{"c1": tool.IDSetDefaultModel}, h.mgr.staged.forSession(h.sessionID))

	h.mgr.runStagedApply(context.Background(), h.sessionID)
	assert.Equal(t, 1, h.restarts)
}

// The apply slot is one, daemon-wide. The marker, the config file and the
// restart an apply ends in are all global, so a second staged change — from any
// session — would overwrite the first and strand the call it belongs to.
func TestStagedCalls_ApplySlotIsDaemonWide(t *testing.T) {
	h := newConfigHarness(t)

	assert.True(t, h.mgr.stageApply(1, "a", tool.IDAddModel, &configops.Staged{}))
	assert.False(t, h.mgr.stageApply(1, "b", tool.IDAddModel, &configops.Staged{}))
	assert.False(t, h.mgr.stageApply(2, "a", tool.IDAddModel, &configops.Staged{}),
		"another session writes the same config file")

	_, _, ok := h.mgr.staged.takePendingApply(1)
	require.True(t, ok)

	assert.False(t, h.mgr.stageApply(1, "b", tool.IDAddModel, &configops.Staged{}),
		"handing the change to the pipeline does not free the slot — only a commit that failed does")

	h.mgr.applier.ReleaseApply()
	assert.True(t, h.mgr.stageApply(1, "b", tool.IDAddModel, &configops.Staged{}))
}

// panicSession is a session whose loop dies the way a bug in it would: the
// runner's recovery keeps the daemon alive, so whatever the loop was holding is
// held for the rest of the process image.
type panicSession struct{ *mockSession }

func (p *panicSession) RunDaemon(
	context.Context,
	func(sessionevent.Notification),
) (session.RunResult, error) {
	panic("session loop bug")
}

// The apply slot is process-global and is given back by exactly two things: a
// commit that failed, or the restart a commit that landed causes. A loop that
// dies in between does neither — so the runner teardown has to, or no session
// and no bootstrap op can ever change the config again on this image.
func TestRunSession_ALoopThatDiesAfterClaimingGivesTheApplySlotBack(t *testing.T) {
	ctx := context.Background()
	h := newConfigHarness(t)

	defer h.mgr.Shutdown(5 * time.Second)

	sessionID := h.liveSession(t)
	tools := make(map[string]tool.Tool)

	for _, tl := range newConfigTools(h.mgr, sessionID) {
		tools[tl.ID()] = tl
	}

	_, err := tools[tool.IDSetDefaultModel].Execute(
		tool.WithCallID(ctx, "c1"), json.RawMessage(`{"id":"claude-opus-5"}`),
	)
	require.ErrorIs(t, err, tool.ErrSuspend)

	h.factory.nextSess = &panicSession{mockSession: &mockSession{}}
	require.NoError(t, h.mgr.SendToSession(ctx, sessionID, "carry on"))

	require.Eventually(t, func() bool {
		return !h.mgr.staged.has(sessionID)
	}, 5*time.Second, 10*time.Millisecond, "the call the dead loop owed is never answered")

	assert.True(t, h.mgr.stageApply(sessionID, "c2", tool.IDAddModel, &configops.Staged{}),
		"the apply slot was never given back")
	assert.Equal(t, toolConfig, h.configBytes(t), "a change that died before the commit writes nothing")
}

// Every tool's params have to reach the config bytes. One success path each,
// because a tool that stages the wrong op fails silently — the verdict says
// "applied" either way.
func TestConfigTool_ParamsReachTheConfig(t *testing.T) {
	tests := []struct {
		name   string
		id     string
		params string
		want   string
	}{
		{
			name:   "set_provider",
			id:     tool.IDSetProvider,
			params: `{"name":"second","driver":"anthropic","api_key":"${WORK_API_KEY}","catalog":"anthropic"}`,
			want:   "catalog: anthropic",
		},
		{
			name:   "add_model",
			id:     tool.IDAddModel,
			params: `{"id":"claude-haiku-4-5","provider":"work"}`,
			want:   "id: claude-haiku-4-5",
		},
		{
			name:   "set_model_tags",
			id:     tool.IDSetModelTags,
			params: `{"id":"claude-opus-5","tags":["coding","review","coding"]}`,
			want:   "tags:\n        - coding\n        - review",
		},
		{
			name:   "remove_model with a replacement default",
			id:     tool.IDRemoveModel,
			params: `{"id":"claude-sonnet-5","new_default":"claude-opus-5"}`,
			want:   "- id: claude-opus-5",
		},
		{
			name:   "set_manager",
			id:     tool.IDSetManager,
			params: `{"id":"tg","driver":"telegram","bot_token":"${WORK_API_KEY}","allowed_user_ids":[7],"target_chat_id":-100}`,
			want:   "target_chat_id: -100",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newConfigHarness(t)

			require.ErrorIs(t, h.call(t, tt.id, "c1", tt.params), tool.ErrSuspend)
			h.mgr.runStagedApply(context.Background(), h.sessionID)

			assert.Contains(t, h.configBytes(t), tt.want)
		})
	}
}

func TestConfigTool_SetModelTagsDeliversOneVerdictAfterRestart(t *testing.T) {
	h := newConfigHarness(t)

	require.ErrorIs(
		t,
		h.call(t, tool.IDSetModelTags, "tags-1", `{"id":"claude-opus-5","tags":["coding"]}`),
		tool.ErrSuspend,
	)
	h.mgr.runStagedApply(context.Background(), h.sessionID)
	h.restart(t, "tags-1", tool.IDSetModelTags)

	assert.False(t, h.mgr.staged.has(h.sessionID))
	assert.Equal(t, 1, h.restarts)
	messages, err := h.sessions.LoadActiveMessages(context.Background(), h.sessionID)
	require.NoError(t, err)
	var verdicts int
	for _, message := range messages {
		if message.ToolName == tool.IDSetModelTags && message.Content == "Config applied." {
			verdicts++
		}
	}
	assert.Equal(t, 1, verdicts)
}

// remove_provider and remove_manager both need a success path, and each has a
// precondition the shared table cannot set up.
func TestConfigTool_RemovalSuccessPaths(t *testing.T) {
	t.Run("remove_provider once nothing references it", func(t *testing.T) {
		ctx := context.Background()
		h := newConfigHarness(t)

		require.ErrorIs(t, h.call(t, tool.IDSetProvider, "c1",
			`{"name":"second","driver":"anthropic","api_key":"${WORK_API_KEY}"}`), tool.ErrSuspend)
		h.mgr.runStagedApply(ctx, h.sessionID)
		h.restart(t, "c1", tool.IDSetDefaultModel)

		require.ErrorIs(t, h.call(t, tool.IDRemoveProvider, "c2", `{"name":"second"}`), tool.ErrSuspend)
		h.mgr.runStagedApply(ctx, h.sessionID)

		assert.NotContains(t, h.configBytes(t), "second")
	})

	t.Run("remove_manager", func(t *testing.T) {
		ctx := context.Background()
		h := newConfigHarness(t)

		require.ErrorIs(t, h.call(
			t,
			tool.IDSetManager,
			"c1",
			`{"id":"tg","driver":"telegram","bot_token":"${WORK_API_KEY}","allowed_user_ids":[7],"target_chat_id":-100}`,
		),
			tool.ErrSuspend)
		h.mgr.runStagedApply(ctx, h.sessionID)
		h.restart(t, "c1", tool.IDSetDefaultModel)

		require.ErrorIs(t, h.call(t, tool.IDRemoveManager, "c2", `{"name":"tg"}`), tool.ErrSuspend)
		h.mgr.runStagedApply(ctx, h.sessionID)

		assert.NotContains(t, h.configBytes(t), "managers:")
	})
}
