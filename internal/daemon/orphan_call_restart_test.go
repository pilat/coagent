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
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/tool"
)

const orphanTaskCallID = "orphan-task-1"

// modelRequests records every transcript the provider was shown, so a test can
// assert no request ever carried a dangling tool_use.
type modelRequests struct {
	mu   sync.Mutex
	seen [][]llmwire.Message
}

// askForBlockingTaskRespond parks the session on a blocking child once, then reacts
// to whatever came back for it.
func askForBlockingTaskRespond(_ string, msgs []llmwire.Message) *llmwire.Response {
	if hasToolResultFor(msgs, tool.IDTask) {
		return &llmwire.Response{Text: "noted: " + lastToolResultContent(msgs, tool.IDTask)}
	}

	return &llmwire.Response{ToolCalls: []llmwire.ToolCall{{
		ID:        orphanTaskCallID,
		Name:      tool.IDTask,
		Arguments: []byte(`{"prompt":"do the thing","description":"orphan probe","subagent_type":"general"}`),
	}}}
}

// newExternalCallDaemon is one daemon image with a config applier wired, so a
// session can suspend on an external call and on config_edit.
func newExternalCallDaemon(
	t *testing.T,
	dbPath, configDir string,
	respond func(string, []llmwire.Message) *llmwire.Response,
) *applyDaemon {
	t.Helper()

	h := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	ops := configops.New(filepath.Join(configDir, "config.yaml"), filepath.Join(configDir, "secrets"))
	restarts := make(chan struct{}, 4)

	h.mgr.applier = configapply.New(ops, func() { restarts <- struct{}{} })

	return &applyDaemon{subagentHarness: h, ops: ops, restarts: restarts}
}

// stageTaskAndStop parks a session on a blocking child, marks the child link
// killed (a child that did not survive the shutdown), and takes the daemon down
// with the task call dangling in the transcript.
func stageTaskAndStop(t *testing.T, dbPath, configDir string, seen *modelRequests) int64 {
	t.Helper()

	first := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForBlockingTaskRespond))

	sessionID, err := first.mgr.Send(
		first.ctx, first.projectID, "do work then spawn", "fake-model", nil,
	)
	require.NoError(t, err)

	first.waitUntil("the parent parked on the child", func() bool {
		return countAssistantToolCallsFor(first.parentMessages(sessionID), tool.IDTask) == 1 &&
			!first.mgr.HasActiveLoop(sessionID)
	})

	msgs := first.parentMessages(sessionID)
	require.Equal(t, 1, countAssistantToolCallsFor(msgs, tool.IDTask))
	require.Zero(t, countToolResultsFor(msgs, tool.IDTask), "the task is out with the world")

	link, err := first.links.GetLinkByTaskCallID(first.ctx, sessionID, orphanTaskCallID)
	require.NoError(t, err)
	require.NotNil(t, link, "the suspended parent owes its call to a child link")

	// The producer ledger is the link row; a child that did not survive the
	// restart owns nothing, so its link must not claim the call either.
	_, err = first.db.ExecContext(
		first.ctx, `DELETE FROM subagent_links WHERE child_id = ?`, link.ChildID,
	)
	require.NoError(t, err)

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

	t.Run("a task loses its producer and is closed", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "orphan.db")
		configDir := newApplyConfigDir(t)

		var seen modelRequests

		sessionID := stageTaskAndStop(t, dbPath, configDir, &seen)

		second := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForBlockingTaskRespond))
		defer second.shutdown()

		require.NoError(t, second.mgr.Start(second.ctx))

		assertAgrees(t, second, sessionID)

		msgs := second.parentMessages(sessionID)
		require.NoError(t, llm.ValidateToolPairing(msgs))
		assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDTask),
			"the orphaned task is closed exactly once")
	})

	t.Run("a task keeps its live child", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "task.db")
		configDir := newApplyConfigDir(t)

		var seen modelRequests

		first := newExternalCallDaemon(t, dbPath, configDir, seen.wrap(askForBlockingTaskRespond))

		sessionID, err := first.mgr.Send(first.ctx, first.projectID, "do work then spawn", "fake-model", nil)
		require.NoError(t, err)

		first.waitUntil("the parent parked on the child", func() bool {
			return countAssistantToolCallsFor(first.parentMessages(sessionID), tool.IDTask) == 1 &&
				!first.mgr.HasActiveLoop(sessionID)
		})

		// No shutdown: the child link is still live, so the sweep must leave
		// the call pending instead of closing it as orphaned.
		first.mgr.sweep(first.ctx)

		assertAgrees(t, first, sessionID)
		assert.Zero(t, countToolResultsFor(first.parentMessages(sessionID), tool.IDTask),
			"a call whose child survived must stay pending")

		first.shutdown()
	})

	t.Run("a config apply keeps its marker", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "apply.db")
		configDir := newApplyConfigDir(t)

		sessionID := stageApplyAndStop(t, dbPath, configDir)

		second := newApplyDaemon(t, dbPath, configDir)
		defer second.shutdown()

		second.mgr.sweep(second.ctx)

		assertAgrees(t, second, sessionID)
		assert.Zero(t, countToolResultsFor(second.parentMessages(sessionID), tool.IDConfigEdit),
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
		assert.Equal(t, 1, countToolResultsFor(msgs, tool.IDConfigEdit),
			"a verdict nobody can produce must be closed, not left dangling")

		// The apply slot is in-memory, so a claim the previous image never gave
		// back cannot reach this one: only a strand inside one image is dangerous.
		assert.True(t, second.mgr.stageApply(sessionID, "later", tool.IDConfigEdit, &configops.Staged{}),
			"a new image starts with a free apply slot")
	})
}
