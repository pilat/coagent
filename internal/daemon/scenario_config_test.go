package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/llm/llmtest"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

func TestScenario_ConfigEditAnswersWithAnOrdinaryResultBeforeRestart(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDirWith(t, toolConfig)
	first := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
	sessionID := startConfigEditSession(t, first, "reconfigure the daemon")
	first.waitUntil("restart requested", func() bool {
		select {
		case <-first.restarts:
			return true
		default:
			return false
		}
	})
	msgs := first.messages(sessionID)
	require.Equal(t, 1, countToolResultsFor(msgs, tool.IDConfigEdit))
	assert.Contains(t, lastToolResultContent(msgs, tool.IDConfigEdit), "Config applied")
	assert.Contains(t, lastToolResultContent(msgs, tool.IDConfigEdit), "restarts")
	require.NotEmpty(t, msgs)
	assert.Equal(t, llmwire.RoleAssistant, msgs[len(msgs)-1].Role)
	assert.Equal(t, "configuration replaced", msgs[len(msgs)-1].Content)
	require.NoError(t, llmtest.ValidateToolPairing(msgs))
	var receipts int
	for _, row := range first.outbox(sessionID) {
		if row.Content == "Config applied — the daemon restarts to load it." {
			receipts++
		}
		assert.NotContains(t, row.Content, "was not changed")
	}
	assert.Equal(t, 1, receipts)
	assert.Contains(t, configBytesOf(t, configDir), "id: claude-opus-5")
	assert.Contains(t, configBytesOf(t, configDir), "${WORK_API_KEY}")
	pending, err := first.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, sessionID, pending.SessionID)
	first.shutdown()
	second := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
	outcome, err := second.ops.ResolvePending(*pending, nil)
	require.NoError(t, err)
	require.True(t, outcome.Verdict.Applied, outcome.Verdict.Reason())
	assert.False(t, outcome.RolledBack)
	require.NoError(t, second.ops.ClearPending(outcome.Pending))
	require.NoError(t, second.mgr.Start(second.ctx))
	second.waitUntil("session idle after restart", func() bool { return !second.mgr.HasActiveLoop(sessionID) })
	inputs, err := second.store.ListPending(second.ctx, sessionID)
	require.NoError(t, err)
	assert.Empty(t, inputs)
	msgs = second.messages(sessionID)
	require.NoError(t, llmtest.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit))
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDConfigEdit))
}

func TestScenario_ASecondSessionCannotOverwriteAStagedApply(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDir(t)
	first := newApplyDaemonWith(t, dbPath, configDir, twoSessionApplyRespond)
	first.startInboxWake()
	sessionA := openConfigRoot(t, first, "telegram:main", "APPLY_A switch the default model")
	require.NoError(t, first.mgr.sendToSession(first.ctx, sessionA, configapply.ConfigEditCommand))
	first.waitUntil("restart requested", func() bool {
		select {
		case <-first.restarts:
			return true
		default:
			return false
		}
	})
	first.waitUntil("A finished its reply", func() bool { return !first.mgr.HasActiveLoop(sessionA) })
	sessionB := openConfigRoot(t, first, "telegram:other", "APPLY_B switch the default model")
	require.NoError(t, first.mgr.sendToSession(first.ctx, sessionB, configapply.ConfigEditCommand))
	first.waitUntil("B answered", func() bool { return !first.mgr.HasActiveLoop(sessionB) })
	pending, err := first.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, sessionA, pending.SessionID)
	assert.Equal(t, "claude-opus-5", defaultModelInFile(t, configDir))
	assert.Zero(t, first.restartCount(), "a refused apply never asks for a second restart")
	msgsB := first.messages(sessionB)
	require.NoError(t, llmtest.ValidateToolPairing(msgsB))
	assert.Equal(t, 1, countToolResultsFor(msgsB, tool.IDConfigEdit))
	assert.Contains(
		t,
		lastToolResultContent(msgsB, tool.IDConfigEdit),
		"another config change is already being applied",
	)
	msgsA := first.messages(sessionA)
	require.NoError(t, llmtest.ValidateToolPairing(msgsA))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgsA, tool.IDConfigEdit))
	assert.Equal(t, 1, countToolResultsFor(msgsA, tool.IDConfigEdit))
	assert.Contains(t, lastToolResultContent(msgsA, tool.IDConfigEdit), "Config applied")
}

// Concurrent applies have one marker owner; the loser is refused inline and neither call remains dangling.
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
			first.startInboxWake()
			id, err := first.mgr.Send(
				first.ctx,
				first.projectID,
				prompt,
				"fake-model",
				managerAttrs("telegram:"+prompt[:7]),
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
	first.waitUntil("both openers settled", func() bool {
		return !first.mgr.HasActiveLoop(sessionA) && !first.mgr.HasActiveLoop(sessionB)
	})
	first.waitUntil("session idle", func() bool { return !first.mgr.HasActiveLoop(sessionA) })
	first.waitUntil("session idle", func() bool { return !first.mgr.HasActiveLoop(sessionB) })
	for _, session := range []int64{sessionA, sessionB} {
		first.startInboxWake()
		require.NoError(t, first.mgr.sendToSession(first.ctx, session, configapply.ConfigEditCommand))
	}
	first.waitUntil("restart requested", func() bool {
		select {
		case <-first.restarts:
			return true
		default:
			return false
		}
	})
	first.waitUntil("both sessions settled", func() bool {
		return !first.mgr.HasActiveLoop(sessionA) && !first.mgr.HasActiveLoop(sessionB)
	})
	assert.Zero(t, first.restartCount(), "one apply, one restart")
	pending, err := first.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)

	winner, loser := sessionA, sessionB
	if pending.SessionID == sessionB {
		winner, loser = sessionB, sessionA
	}
	require.Equal(t, winner, pending.SessionID)
	msgsLoser := first.messages(loser)
	require.NoError(t, llmtest.ValidateToolPairing(msgsLoser))
	assert.Equal(t, 1, countToolResultsFor(msgsLoser, tool.IDConfigEdit))
	assert.Contains(
		t,
		lastToolResultContent(msgsLoser, tool.IDConfigEdit),
		"another config change is already being applied",
	)
	msgsWinner := first.messages(winner)
	require.NoError(t, llmtest.ValidateToolPairing(msgsWinner))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgsWinner, tool.IDConfigEdit))
	assert.Equal(t, 1, countToolResultsFor(msgsWinner, tool.IDConfigEdit))
	assert.Contains(t, lastToolResultContent(msgsWinner, tool.IDConfigEdit), "Config applied")
}

func TestScenario_ConfigApplyRejectionReachesTheSessionInProcess(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDir(t)
	d := newApplyDaemon(t, dbPath, configDir)
	d.mgr.applier = configapply.New(failingCommitOps{d.ops})
	sessionID := startConfigEditSession(t, d, "switch the default model")
	d.waitUntil("rejection stored", func() bool {
		return countToolResultsFor(d.messages(sessionID), tool.IDConfigEdit) == 1
	})
	d.waitUntil("session idle", func() bool { return !d.mgr.HasActiveLoop(sessionID) })
	msgs := d.messages(sessionID)
	require.NoError(t, llmtest.ValidateToolPairing(msgs))
	assert.Contains(t, lastToolResultContent(msgs, tool.IDConfigEdit), "rejected")
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit))
	assert.Zero(t, d.restartCount(), "a rejected commit never restarts")
	assert.NoFileExists(t, filepath.Join(configDir, coagenthome.PendingApplyFileName))
}

func TestScenario_ConfigQuestionCannotReplaceDocumentWithSandboxFragment(t *testing.T) {
	configDir := newApplyConfigDir(t)
	configPath := filepath.Join(configDir, "config.yaml")
	before, err := os.ReadFile(configPath)
	require.NoError(t, err)
	d := newApplyDaemonWith(t, filepath.Join(t.TempDir(), "config-question.db"), configDir,
		func(_ string, messages []llmwire.Message) *llmwire.Response {
			if hasToolResultFor(messages, tool.IDConfigEdit) {
				return textReply("configuration unchanged")
			}
			if hasUserContaining(messages, "/config") {
				return callReply(
					"fragment", tool.IDConfigEdit,
					`{"document":"sandbox:\n  enabled: true\n  projects:\n    /tmp/project:\n      rules: []\n"}`,
				)
			}
			return textReply("ready")
		})
	d.startInboxWake()
	id := openConfigRoot(t, d, "telegram:main", "hello")
	d.waitUntil("opening turn settled", func() bool { return !d.mgr.HasActiveLoop(id) })
	require.NoError(t, d.mgr.sendToSession(d.ctx, id, "/config - is the sandbox unconfigured?"))
	d.waitUntil("invalid replacement answered", func() bool {
		return hasToolResultFor(d.messages(id), tool.IDConfigEdit) && !d.mgr.HasActiveLoop(id)
	})
	after, err := os.ReadFile(configPath)
	require.NoError(t, err)
	assert.Equal(t, before, after)
	assert.Zero(t, d.restartCount())
	pending, err := d.ops.LoadPending()
	require.NoError(t, err)
	assert.Nil(t, pending)
	assert.Contains(t, lastToolResultContent(d.messages(id), tool.IDConfigEdit), "configuration fragments")
}

// Without a user /config turn the model sees config_edit but gets an authorization error before anything is staged.
func TestScenario_ConfigEditWithoutActivationNeverStages(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDirWith(t, toolConfig)
	d := newApplyDaemonWith(t, dbPath, configDir, unauthorizedConfigEditRespond)
	require.NoError(t, d.mgr.Start(d.ctx))

	// The responder calls config_edit; no activation exists, so the session must
	// be answered with the authorization refusal, in-process.
	d.startInboxWake()
	sessionID := openConfigRoot(t, d, "telegram:main", "reconfigure the daemon")
	d.waitUntil("refusal stored", func() bool {
		return countToolResultsFor(d.messages(sessionID), tool.IDConfigEdit) == 1
	})
	d.waitUntil("session idle", func() bool { return !d.mgr.HasActiveLoop(sessionID) })
	msgs := d.messages(sessionID)
	require.NoError(t, llmtest.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit))
	assert.Contains(t, lastToolResultContent(msgs, tool.IDConfigEdit), "/config")
	assert.Zero(t, d.restartCount(), "an unauthorized call never applies")
	assert.Equal(t, toolConfig, configBytesOf(t, configDir))
}

func TestScenario_ConfigEditBootInvalidCandidateRollsBack(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDirWith(t, toolConfig)
	first := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
	sessionID := startConfigEditSession(t, first, "reconfigure the daemon")
	first.waitUntil("restart requested", func() bool {
		select {
		case <-first.restarts:
			return true
		default:
			return false
		}
	})
	pending, err := first.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)
	first.shutdown()
	second := newApplyDaemonWith(t, dbPath, configDir, configEditRespond)
	outcome, err := second.ops.ResolvePending(*pending, errors.New("cold catalog: unknown model claude-opus-9"))
	require.NoError(t, err)
	require.True(t, outcome.Verdict.Failed())
	assert.True(t, outcome.RolledBack)
	require.NoError(t, second.ops.ClearPending(outcome.Pending))
	msgs := second.messages(sessionID)
	require.NoError(t, llmtest.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDConfigEdit))
	assert.Contains(t, lastToolResultContent(msgs, tool.IDConfigEdit), "Config applied")
	assert.Equal(t, toolConfig, configBytesOf(t, configDir), "the rollback restores the previous config")
}

func TestConfigTool_SuccessWritesAndAnswers(t *testing.T) {
	h := newConfigHarness(t)
	result, err := h.tools[tool.IDConfigEdit].Execute(
		grantedCall(t.Context(), h.sessionID, "c1"), configEditArgs(configHarnessCandidate),
	)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Contains(t, result.Output, "Config applied")
	assert.Contains(t, result.Output, "ENTIRE configuration")
	assert.Equal(t, []string{"Config applied — the daemon restarts to load it."}, result.DirectMessages)
	assert.Contains(t, h.configBytes(t), "id: claude-opus-5\n      provider: work\n    - id: claude-sonnet-5")
	pending, err := h.mgr.applier.Ops().LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)
	assert.Equal(t, h.sessionID, pending.SessionID)
	assert.Zero(t, h.restartCount())
	h.mgr.applier.RestartIfApplied(h.sessionID + 1)
	assert.Zero(t, h.restartCount())
	h.mgr.applier.RestartIfApplied(h.sessionID)
	h.mgr.applier.RestartIfApplied(h.sessionID)
	assert.Equal(t, 1, h.restartCount())
}

func TestConfigTool_GuardViolationsAreImmediateErrors(t *testing.T) {
	h := newConfigHarness(t)
	t.Run("missing activation grant", func(t *testing.T) {
		h.recordCall(t, "c-grant", tool.IDConfigEdit)
		_, err := h.tools[tool.IDConfigEdit].Execute(
			tool.WithCallID(context.Background(), "c-grant"), configEditArgs(configHarnessCandidate),
		)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "/config")
	})
	t.Run("invalid document", func(t *testing.T) {
		err := h.grantedCall(t, "c-var", "providers: [unclosed\n")
		require.Error(t, err)
	})
	t.Run("empty document", func(t *testing.T) {
		err := h.grantedCall(t, "c-doc", "")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "document is required")
	})
	assert.Equal(t, 0, h.restartCount())
	assert.Equal(t, toolConfig, h.configBytes(t))
}

func TestConfigTool_TwoAppliesInSequence(t *testing.T) {
	h := newConfigHarness(t)
	require.NoError(t, h.grantedCall(t, "c1", configHarnessCandidate))
	h.mgr.applier.RestartIfApplied(h.sessionID)
	assert.Equal(t, 1, h.restartCount())
	pending, err := h.mgr.applier.Ops().LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)
	require.NoError(t, h.mgr.applier.Ops().ClearPending(*pending))
	h.mgr.applier = configapply.New(h.mgr.applier.Ops())
	h.tools[tool.IDConfigEdit] = configapply.NewConfigEdit(h.sessionID, h.mgr.applier)
	require.NoError(t, h.grantedCall(t, "c2", toolConfig))
	assert.Contains(t, h.configBytes(t), "id: claude-sonnet-5\n      provider: work\n    - id: claude-opus-5")
	h.mgr.applier.RestartIfApplied(h.sessionID)
	assert.Equal(t, 1, h.restartCount(), "the new process image signals its own restart")
}

// The tool description states the restart contract a caller cannot see.
func TestConfigTool_DescriptionsCarryTheContract(t *testing.T) {
	h := newConfigHarness(t)
	assert.Contains(t, h.tools[tool.IDConfigEdit].Description(), "restarts the daemon")
}

func TestConfigTool_RefusesASecondApplyInTheSameTurn(t *testing.T) {
	h := newConfigHarness(t)
	require.NoError(t, h.grantedCall(t, "c1", configHarnessCandidate))
	err := h.grantedCall(t, "c2", toolConfig)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "another config change is already being applied")
	assert.Contains(t, err.Error(), "one change at a time")
	h.mgr.applier.RestartIfApplied(h.sessionID)
	assert.Equal(t, 1, h.restartCount())
}

func TestConfigTool_WholeDocumentReachesTheConfig(t *testing.T) {
	h := newConfigHarness(t)
	document := toolConfig + "    - id: claude-haiku-4-5\n      provider: work\n"
	require.NoError(t, h.grantedCall(t, "c1", document))
	assert.Contains(t, h.configBytes(t), "id: claude-haiku-4-5")
}

func TestScenario_ConfigDocumentThroughHTTPPreservesModelIDs(t *testing.T) {
	for _, damaged := range []bool{false, true} {
		t.Run(
			map[bool]string{false: "intact response", true: "damaged response rejected"}[damaged],
			func(t *testing.T) {
				testConfigDocumentThroughHTTP(t, damaged)
			},
		)
	}
}

// A model switch persists its own default effort because the next run reads only the session record.
func TestSetModelRecordsTheEffortTheNextRunSends(t *testing.T) {
	provider := newEffortProvider(t)
	h := newHarness(
		t, harnessOptions{
			configure: withEffortModels(provider.url),
			clientFor: configuredClient(withEffortModels(provider.url)),
		},
	)
	h.startInboxWake()
	id, err := h.mgr.Send(h.ctx, h.projectID, "first", "plain-model", nil)
	require.NoError(t, err)
	// The two-phase check spends a hidden candidate and a confirmation, both
	// "answer N" from the stub, so one visible turn is two assistant rows.
	h.waitUntil("first turn answered", func() bool {
		replyCount := 0
		for _, m := range h.messages(id) {
			if m.Role == llmwire.RoleAssistant && m.Content != "" {
				replyCount++
			}
		}
		return replyCount == 2
	})
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(id) })
	require.NoError(t, h.mgr.SetModel(h.ctx, id, "thinker", ""))
	rec := h.session(id)
	assert.Equal(t, "high", rec.ReasoningLevel,
		"an unnamed level settles on the model's default, and the record keeps that")
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, id, "second"))
	h.waitUntil("second turn answered", func() bool {
		replyCount := 0
		for _, m := range h.messages(id) {
			if m.Role == llmwire.RoleAssistant && m.Content != "" {
				replyCount++
			}
		}
		return replyCount == 4
	})
	h.waitUntil("session idle", func() bool { return !h.mgr.HasActiveLoop(id) })
	reqs := provider.snapshot()
	require.Len(t, reqs, 4)
	assert.Equal(t, "plain-model", reqs[0].model)
	assert.Empty(t, reqs[0].effort, "a model with no effort selector carries none")
	assert.Equal(t, "thinker", reqs[2].model)
	assert.Equal(t, "high", reqs[2].effort,
		"the run recreated from the record must ask for the level the switch settled on")
}

const (
	applyCallA = "cfg-call-a"
	applyCallB = "cfg-call-b"
)

// Distinct opener prompts and call IDs reveal a stranded verdict when two root sessions contend for one config apply.
func twoSessionApplyRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if hasToolResultFor(msgs, tool.IDConfigEdit) {
		return textReply("configuration replaced")
	}
	if !hasConfigInput(msgs) {
		return textReply("ready to reconfigure")
	}
	if hasUserContaining(msgs, "APPLY_B") {
		return callReply(applyCallB, tool.IDConfigEdit, `{"document":`+mustQuoteJSON(configEditCandidateB)+`}`)
	}
	return callReply(applyCallA, tool.IDConfigEdit, `{"document":`+mustQuoteJSON(configEditCandidateA)+`}`)
}

const configEditCandidateA = `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
models:
    - id: claude-opus-5
      provider: work
    - id: claude-sonnet-5
      provider: work
`

const configEditCandidateB = `providers:
    work:
        driver: anthropic
        api_key: ${WORK_API_KEY}
models:
    - id: claude-sonnet-5
      provider: work
    - id: claude-opus-5
      provider: work
`

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

// failingCommitOps is an ops layer whose write fails, so the apply is rejected
// with no restart and no marker — the verdict has to come back inline.
type failingCommitOps struct {
	configops.Service
}

func (failingCommitOps) Commit(*configops.Staged, configops.Pending) configops.Verdict {
	return configops.Reject("", errors.New("no space left on device"))
}

const configEditCallID = "cfg-edit-call-1"

func configEditRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if hasToolResultFor(msgs, tool.IDConfigEdit) {
		return textReply("configuration replaced")
	}
	if hasConfigInput(msgs) {
		return callReply(configEditCallID, tool.IDConfigEdit, `{"document":`+mustQuoteJSON(configEditCandidate)+`}`)
	}
	return textReply("ready to reconfigure")
}

// unauthorizedConfigEditRespond calls config_edit with no /config turn, so the
// session must answer the authorization refusal in-process.
func unauthorizedConfigEditRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if hasToolResultFor(msgs, tool.IDConfigEdit) {
		return textReply("configuration replaced")
	}
	return callReply(configEditCallID, tool.IDConfigEdit, `{"document":`+mustQuoteJSON(configEditCandidate)+`}`)
}

func hasConfigInput(messages []llmwire.Message) bool {
	return hasUserContaining(messages, configapply.ConfigEditCommand)
}

// Settle the opener so the command cannot race the initial model call.
func startConfigEditSession(t *testing.T, d *applyDaemon, prompt string) int64 {
	t.Helper()
	d.startInboxWake()
	sessionID := openConfigRoot(t, d, "telegram:main", prompt)
	require.NoError(t, d.mgr.sendToSession(d.ctx, sessionID, configapply.ConfigEditCommand))
	return sessionID
}

func openConfigRoot(t *testing.T, d *applyDaemon, owner, prompt string) int64 {
	t.Helper()
	id, err := d.mgr.Send(d.ctx, d.projectID, prompt, "fake-model", managerAttrs(owner))
	require.NoError(t, err)
	d.waitUntil("root opener settled", func() bool { return !d.mgr.HasActiveLoop(id) })
	return id
}

func configBytesOf(t *testing.T, configDir string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(configDir, "config.yaml"))
	require.NoError(t, err)
	return string(body)
}

type configHarness struct {
	*harness
	sessionID int64
	sessions  *sessionstore.Store
	tools     map[string]tool.Tool
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
	factory := &mockFactory{}
	base := newHarness(t, harnessOptions{configure: withTestModels, clientFor: factory.client})
	h := &configHarness{harness: base, config: configPath}
	mgr, store := base.mgr, base.store
	sessions, ok := mgr.store.(*sessionstore.Store)
	require.True(t, ok)
	h.sessions = sessions
	projectID, err := store.GetOrCreateProject(context.Background(), t.TempDir())
	require.NoError(t, err)
	h.projectID = projectID
	mgr.applier = configapply.New(configops.New(configPath, secretsPath))
	h.mgr = mgr
	h.sessionID = h.liveSession(t)
	h.tools = map[string]tool.Tool{tool.IDConfigEdit: configapply.NewConfigEdit(h.sessionID, mgr.applier)}
	return h
}

// Reordering the models makes document replacement visible on disk.
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

func grantedCall(ctx context.Context, sessionID int64, callID string) context.Context {
	ctx = tool.WithCallID(ctx, callID)
	return tool.WithActivationGrant(ctx, tool.ActivationGrant{
		SessionID: sessionID, ToolID: tool.IDConfigEdit, Command: "/config",
	})
}

func configEditArgs(document string) json.RawMessage {
	encoded, err := json.Marshal(document)
	if err != nil {
		panic(err)
	}
	return json.RawMessage(`{"document":` + string(encoded) + `}`)
}

func (h *configHarness) grantedCall(t *testing.T, callID, document string) error {
	t.Helper()
	h.recordCall(t, callID, tool.IDConfigEdit)
	_, err := h.tools[tool.IDConfigEdit].Execute(
		grantedCall(t.Context(), h.sessionID, callID), configEditArgs(document),
	)
	return err
}

// liveSession creates a real session record, so notification delivery has somewhere to land.
func (h *configHarness) liveSession(t *testing.T) int64 {
	t.Helper()
	rec := h.createRoot(map[string]any{"channel": "cli", "manager_id": "cli"})
	return rec
}

func (h *configHarness) recordCall(t *testing.T, callID, toolName string) {
	t.Helper()
	calls, err := json.Marshal([]llmwire.ToolCall{{ID: callID, Name: toolName}})
	require.NoError(t, err)
	_, err = h.sessions.Commit(t.Context(), sessionstore.Commit{
		SessionID: h.sessionID, Messages: []*transcript.Message{{Role: llmwire.RoleAssistant, ToolCalls: calls}},
	})
	require.NoError(t, err)
}

func (h *configHarness) restartCount() int { return len(h.mgr.applier.Restart()) }

func (h *configHarness) configBytes(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(h.config)
	require.NoError(t, err)
	return string(data)
}

type configWireMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Name    string `json:"name"`
}

func testConfigDocumentThroughHTTP(t *testing.T, damaged bool) {
	t.Helper()
	initial := strings.ReplaceAll(toolConfig, "claude-sonnet-5", "anthropic/claude-sonnet-4.6")
	initial = strings.ReplaceAll(initial, "claude-opus-5", "anthropic/claude-opus-4.6")
	candidate := initial + "sandbox:\n    rules:\n        - deny: ~/.ssh\n"
	if damaged {
		candidate = strings.ReplaceAll(candidate, "claude-sonnet-4.6", "")
		candidate = strings.ReplaceAll(candidate, "claude-opus-4.6", "")
	}
	configDir := newApplyConfigDirWith(t, initial)
	configPath := filepath.Join(configDir, "config.yaml")
	var sawRead atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Messages []configWireMessage `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		message := map[string]any{"role": "assistant", "content": "ready"}
		finish := "stop"
		command, read, edited := false, false, false
		for _, input := range request.Messages {
			command = command || input.Role == "user" && strings.Contains(input.Content, "/config")
			if input.Role == "tool" && input.Name == tool.IDConfigRead {
				read = true
				sawRead.Store(true)
				assert.Contains(t, input.Content, "anthropic/claude-sonnet-4.6")
				assert.Contains(t, input.Content, "anthropic/claude-opus-4.6")
			}
			edited = edited || input.Role == "tool" && input.Name == tool.IDConfigEdit
		}
		if command && !edited {
			name, args := tool.IDConfigRead, map[string]string{}
			if read {
				name, args = tool.IDConfigEdit, map[string]string{"document": candidate}
			}
			encoded, err := json.Marshal(args)
			assert.NoError(t, err)
			message["tool_calls"] = []any{map[string]any{
				"id": name + "-call", "type": "function",
				"function": map[string]any{"name": name, "arguments": string(encoded)},
			}}
			finish = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		assert.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": message, "finish_reason": finish}},
		}))
	}))
	defer server.Close()
	d := newApplyDaemonWith(t, filepath.Join(t.TempDir(), "transport.db"), configDir, configEditRespond)
	workDir, err := d.mgr.store.GetProjectWorkDir(d.ctx, d.projectID)
	require.NoError(t, err)
	wireConfig := &config.Config{Model: "fake-model", UnifiedConfig: &config.UnifiedConfig{
		Providers: map[string]config.ProviderEntry{"test": {Driver: "openai", BaseURL: server.URL, APIKey: "test-key"}},
		Models:    []config.ModelEntry{{ID: "fake-model", Provider: "test", MaxTokens: 8192, ContextWindow: 100000}},
	}}
	d.mgr.build.Config = wireConfig
	d.mgr.build.WorkDir = workDir
	id := startConfigEditSession(t, d, "hello")
	d.waitUntil("session idle", func() bool { return !d.mgr.HasActiveLoop(id) })
	assert.True(t, sawRead.Load())
	var storedDocuments []string
	for _, message := range d.messages(id) {
		for _, call := range message.ToolCalls {
			if call.Name == tool.IDConfigEdit {
				var args struct {
					Document string `json:"document"`
				}
				require.NoError(t, json.Unmarshal(call.Arguments, &args))
				storedDocuments = append(storedDocuments, args.Document)
			}
		}
	}
	assert.Equal(t, []string{candidate}, storedDocuments)
	actual, err := os.ReadFile(configPath)
	require.NoError(t, err)
	if damaged {
		assert.Equal(t, initial, string(actual))
		assert.Zero(t, d.restartCount())
		assert.Contains(t, configLastToolResultContent(d.messages(id), tool.IDConfigEdit), "duplicate model id")
		return
	}
	assert.Equal(t, candidate, string(actual))
	assert.Equal(t, 1, d.restartCount())
}

func lastToolResultContent(msgs []llmwire.Message, toolName string) string {
	for _, v := range slices.Backward(msgs) {
		if v.Role == llmwire.RoleTool && v.ToolName == toolName {
			return v.Content
		}
	}
	return ""
}

func TestScenario_ConfigEditFromRoot(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "apply.db")
	configDir := newApplyConfigDirWith(t, toolConfig)
	first := newApplyDaemonWith(t, dbPath, configDir, configReadEditRespond)
	sessionID := startConfigEditSession(t, first, "reconfigure the daemon")
	first.waitUntil("restart requested", func() bool {
		select {
		case <-first.restarts:
			return true
		default:
			return false
		}
	})
	pending, err := first.ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)
	require.Equal(t, sessionID, pending.SessionID)
	msgs := first.messages(sessionID)
	require.NoError(t, llmtest.ValidateToolPairing(msgs))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigRead))
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDConfigRead))
	assert.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDConfigEdit))
	assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDConfigEdit))
	assert.Contains(t, lastToolResultContent(msgs, tool.IDConfigEdit), "Config applied")
	require.NotEmpty(t, msgs)
	assert.Equal(t, "configuration replaced", msgs[len(msgs)-1].Content)
	var receipts int
	for _, row := range first.outbox(sessionID) {
		if row.Content == "Config applied — the daemon restarts to load it." {
			receipts++
		}
	}
	assert.Equal(t, 1, receipts)
	assert.Zero(t, first.restartCount(), "exactly one signal was received above")
	assert.Contains(t, configBytesOf(t, configDir), "id: claude-opus-5")
	assert.Contains(t, configBytesOf(t, configDir), "${WORK_API_KEY}")
}

func configReadEditRespond(system string, messages []llmwire.Message) *llmwire.Response {
	if hasConfigInput(messages) && !hasToolResultFor(messages, tool.IDConfigRead) {
		return callReply("read-config", tool.IDConfigRead, `{}`)
	}
	return configEditRespond(system, messages)
}

// effortRequest is what one provider call named: its model and reasoning effort.
type effortRequest struct {
	model  string
	effort string
}

func withEffortModels(baseURL string) func(*config.Config) {
	return func(cfg *config.Config) {
		cfg.Model = "plain-model"
		cfg.UnifiedConfig = &config.UnifiedConfig{
			Providers: map[string]config.ProviderEntry{"or": {Driver: "openrouter", APIKey: "key", BaseURL: baseURL}},
			Models: []config.ModelEntry{
				{ID: "plain-model", Provider: "or", ContextWindow: 200_000, MaxTokens: 64_000},
				{
					ID: "thinker", Provider: "or", ContextWindow: 200_000, MaxTokens: 64_000,
					EffortLevels:  []string{"low", "high"},
					DefaultEffort: "high",
					Reasoning:     &config.ReasoningSpec{Supported: true, Efforts: []string{"low", "high"}},
				},
			},
		}
	}
}

// effortProvider is an OpenRouter-shaped endpoint recording the reasoning effort
// of every call, so what a session actually asks for is observable.
type effortProvider struct {
	url string

	mu       sync.Mutex
	requests []effortRequest
}

func newEffortProvider(t *testing.T) *effortProvider {
	t.Helper()
	p := &effortProvider{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model     string `json:"model"`
			Reasoning struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		p.mu.Lock()
		p.requests = append(p.requests, effortRequest{model: body.Model, effort: body.Reasoning.Effort})
		turn := len(p.requests)
		p.mu.Unlock()
		_, _ = fmt.Fprintf(w,
			`{"choices":[{"message":{"role":"assistant","content":"answer %d"},"finish_reason":"stop"}],"usage":{}}`,
			turn,
		)
	}))
	t.Cleanup(srv.Close)
	p.url = srv.URL
	return p
}

func (p *effortProvider) snapshot() []effortRequest {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]effortRequest(nil), p.requests...)
}

func configLastToolResultContent(msgs []llmwire.Message, toolName string) string {
	for _, v := range slices.Backward(msgs) {
		if v.Role == llmwire.RoleTool && v.ToolName == toolName {
			return v.Content
		}
	}
	return ""
}
