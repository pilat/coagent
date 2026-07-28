package daemon

import (
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

const (
	secretCallID      = "secret-call-1"
	orphanSleepCallID = "sleep-call-orphan"
)

// modelRequests records every transcript the provider was shown, so a test can
// assert no request ever carried a dangling tool_use.
type modelRequests struct {
	mu   sync.Mutex
	seen [][]llmwire.Message
}

// askForSecretRespond asks for a credential once, then reacts to whatever came
// back for it.
func askForSecretRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if hasToolResultFor(msgs, tool.IDRequestSecret) {
		return &llmwire.Response{Text: "noted: " + lastToolResultContent(msgs, tool.IDRequestSecret)}
	}

	return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
		ID:        secretCallID,
		Name:      tool.IDRequestSecret,
		Arguments: []byte(`{"name":"MANAGER_TG_BOT_TOKEN","purpose":"the bot token from BotFather"}`),
	}}}
}

// longSleepRespond parks the session on a timer that outlives the process.
func longSleepRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if hasToolResultFor(msgs, tool.IDSleep) {
		return &llmwire.Response{Text: "awake again"}
	}

	return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
		ID:        orphanSleepCallID,
		Name:      tool.IDSleep,
		Arguments: []byte(`{"duration":"1h","reason":"wait for the world"}`),
	}}}
}

// newExternalCallDaemon is one daemon image with a config applier wired, so a
// terminal session is served request_secret and the config tools.
func newExternalCallDaemon(
	t *testing.T,
	dbPath, configDir string,
	respond func(string, []llmwire.Message) *llmwire.Response,
) *applyDaemon {
	t.Helper()

	h := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	ops := configops.New(filepath.Join(configDir, "config.yaml"), filepath.Join(configDir, "secrets"))
	restarts := make(chan struct{}, 4)

	h.mgr.applier = NewConfigApplier(ops, func() { restarts <- struct{}{} })

	return &applyDaemon{subagentHarness: h, ops: ops, restarts: restarts}
}

// stageSecretRequestAndStop runs a terminal session up to the masked prompt and
// takes the daemon down with nobody having typed anything.
func stageSecretRequestAndStop(t *testing.T, dbPath, configDir string, seen *modelRequests) int64 {
	t.Helper()

	first := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForSecretRespond))

	sessionID, err := first.mgr.Send(
		first.ctx, first.projectID, "set up the telegram bot", "fake-model", map[string]any{"channel": "cli"},
	)
	require.NoError(t, err)

	first.waitUntil("the session suspended on the masked prompt", func() bool {
		return first.mgr.staged.has(sessionID) && !first.mgr.HasActiveLoop(sessionID)
	})

	msgs := first.parentMessages(sessionID)
	require.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDRequestSecret))
	require.Zero(t, countToolResultsFor(msgs, tool.IDRequestSecret), "the prompt is out with the person")

	first.shutdown()

	return sessionID
}

// unresolvedExternalCallsByName is the reference view of what is pending: the
// external calls a provider can see dangling in the transcript, derived from the
// transcript alone.
func unresolvedExternalCallsByName(msgs []llmwire.Message) map[string]string {
	resolved := make(map[string]bool)

	for _, m := range msgs {
		if m.Role == llmwire.RoleTool && m.ToolCallID != "" {
			resolved[m.ToolCallID] = true
		}
	}

	out := make(map[string]string)

	for _, m := range msgs {
		if m.Role != llmwire.RoleAssistant {
			continue
		}

		for _, tc := range m.ToolCalls {
			if tc.ID != "" && !resolved[tc.ID] && tool.IsExternalCall(tc.Name) {
				out[tc.ID] = tc.Name
			}
		}
	}

	return out
}

func (r *modelRequests) wrap(
	respond func(string, []llmwire.Message) *llmwire.Response,
) func(string, []llmwire.Message) *llmwire.Response {
	return func(system string, msgs []llmwire.Message) *llmwire.Response {
		r.mu.Lock()
		r.seen = append(r.seen, slices.Clone(msgs))
		r.mu.Unlock()

		return respond(system, msgs)
	}
}

func (r *modelRequests) assertAllPaired(t *testing.T) {
	t.Helper()

	r.mu.Lock()
	defer r.mu.Unlock()

	for i, req := range r.seen {
		assert.NoErrorf(t, llm.ValidateToolPairing(req), "request %d reached the provider with a dangling tool_use", i)
	}
}

// The invariant, stated as a comparison between two independent views: the
// ledger-keyed and name-keyed pending sets must agree; every pending external
// call has an owner able to resolve it, across restart.
func TestHarnessModel_PendingExternalCallOwnershipAgreesAfterRestart(t *testing.T) {
	assertAgrees := func(t *testing.T, d *applyDaemon, sessionID int64) {
		t.Helper()

		owners, err := d.mgr.pendingExternalCallsForSession(d.ctx, sessionID)
		require.NoError(t, err)

		assert.Equal(t, unresolvedExternalCallsByName(d.parentMessages(sessionID)), owners,
			"an unresolved external call the provider can see must have a producer that can resolve it")
	}

	t.Run("a secret request loses its producer and is closed", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "secret.db")
		configDir := newApplyConfigDir(t)

		var seen modelRequests

		sessionID := stageSecretRequestAndStop(t, dbPath, configDir, &seen)

		second := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForSecretRespond))
		defer second.shutdown()

		second.mgr.sweep(second.ctx)

		assertAgrees(t, second, sessionID)

		msgs := second.parentMessages(sessionID)
		require.NoError(t, llm.ValidateToolPairing(msgs))
		assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDRequestSecret),
			"the orphaned prompt is closed exactly once")
	})

	t.Run("a sleep keeps its durable timer", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "sleep.db")
		configDir := newApplyConfigDir(t)

		first := newExternalCallDaemon(t, dbPath, configDir, longSleepRespond)

		sessionID, err := first.mgr.Send(first.ctx, first.projectID, "wait a while", "fake-model", nil)
		require.NoError(t, err)

		first.waitUntil("the session parked on the timer", func() bool {
			return countAssistantToolCallsFor(first.parentMessages(sessionID), tool.IDSleep) == 1 &&
				!first.mgr.HasActiveLoop(sessionID)
		})
		first.shutdown()

		second := newExternalCallDaemon(t, dbPath, configDir, longSleepRespond)
		defer second.shutdown()

		second.mgr.sweep(second.ctx)

		assertAgrees(t, second, sessionID)
		assert.Zero(t, countToolResultsFor(second.parentMessages(sessionID), tool.IDSleep),
			"a call whose producer survived must stay pending")
	})

	t.Run("a config apply keeps its marker", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "apply.db")
		configDir := newApplyConfigDir(t)

		sessionID := stageApplyAndStop(t, dbPath, configDir)

		second := newApplyDaemon(t, dbPath, configDir)
		defer second.shutdown()

		second.mgr.sweep(second.ctx)

		assertAgrees(t, second, sessionID)
		assert.Zero(t, countToolResultsFor(second.parentMessages(sessionID), tool.IDSetDefaultModel),
			"the marker still owes this call its verdict")

		_, err := second.bootVerdict(t)
		require.NoError(t, err, "the verdict must still reach the call the sweep left alone")
	})

	t.Run("a config apply whose marker did not survive", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "apply-nomarker.db")
		configDir := newApplyConfigDir(t)

		sessionID := stageApplyAndStop(t, dbPath, configDir)
		require.NoError(t, os.Remove(filepath.Join(configDir, coagenthome.PendingApplyFileName)))

		second := newApplyDaemon(t, dbPath, configDir)
		defer second.shutdown()

		second.mgr.sweep(second.ctx)

		assertAgrees(t, second, sessionID)

		msgs := second.parentMessages(sessionID)
		require.NoError(t, llm.ValidateToolPairing(msgs))
		assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDSetDefaultModel),
			"a verdict nobody can produce must be closed, not left dangling")

		// The apply slot is in-memory, so a claim the previous image never gave
		// back cannot reach this one: only a strand inside one image is dangerous.
		assert.True(t, second.mgr.stageApply(sessionID, "later", tool.IDAddModel, &configops.Staged{}),
			"a new image starts with a free apply slot")
	})
}
