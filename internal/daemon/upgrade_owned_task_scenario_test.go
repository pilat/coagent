package daemon

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

// TestScenario_OwnedTaskCallSurvivesSchemaUpgradeAndRestarts stages a version-30
// database: a runnable foreground child whose link owns the parent's task call,
// the link still carrying a legacy nonzero timeout_sec. After the boot migration
// drops the column, the task tool must not re-execute, the link must still own
// the call, and the pre-existing child must complete exactly once.
func TestScenario_OwnedTaskCallSurvivesSchemaUpgradeAndRestarts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "upgrade.db")
	ctx := context.Background()

	// Stage the pre-upgrade state at version 30.
	staged, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	require.NoError(t, migrate.RunUpTo(ctx, staged, 30))

	_, err = staged.ExecContext(ctx, `INSERT INTO projects (id, work_dir, name) VALUES (1, ?, 'p')`, t.TempDir())
	require.NoError(t, err)

	stagedStore := sessionstore.NewStore(staged)
	parent, err := stagedStore.CreateSession(ctx, 1, "fake-model", "", nil)
	require.NoError(t, err)
	require.NoError(t, stagedStore.UpdateSessionStatus(ctx, parent.ID, sessionstore.SessionStatusSuspended))

	// Stage through raw v30-shaped inserts: the v30 schema predates the
	// tool_error column, so the session store's insert (which writes it)
	// cannot run against this staged database.
	_, err = staged.ExecContext(ctx,
		`INSERT INTO messages (session_id, role, content) VALUES (?, 'user', 'kick off the work')`,
		parent.ID)
	require.NoError(t, err)

	// Arguments serialize exactly as the session store persists them, with the
	// legacy "timeout" key the current schema no longer decodes into anything.
	stagedArgs, err := json.Marshal(llmwire.ToolCall{
		ID: "tc_staged", Name: tool.IDTask, Arguments: []byte(`{"prompt":"CHILD_STAGED","timeout":1}`),
	})
	require.NoError(t, err)
	_, err = staged.ExecContext(ctx,
		`INSERT INTO messages (session_id, role, content, tool_calls) VALUES (?, 'assistant', '', ?)`,
		parent.ID, "["+string(stagedArgs)+"]")
	require.NoError(t, err)

	result, err := staged.ExecContext(ctx, `INSERT INTO sessions
		(project_id, parent_id, root_id, agent_type, model, reasoning_level, created_at, updated_at)
		VALUES (1, ?, ?, 'general', 'fake-model', 'medium', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		parent.ID, parent.ID)
	require.NoError(t, err)
	childID, err := result.LastInsertId()
	require.NoError(t, err)
	_, err = staged.ExecContext(ctx, `INSERT INTO subagent_links
		(parent_id, child_id, task_call_id, blocking, depth, state, created_at, timeout_sec)
		VALUES (?, ?, 'tc_staged', 1, 1, 'spawned', 1, 1)`, parent.ID, childID)
	require.NoError(t, err)
	_, err = staged.ExecContext(ctx, `INSERT INTO session_inbox
		(session_id, source, raw_content, received_at)
		VALUES (?, 'agent', 'CHILD_STAGED do the staged work', CURRENT_TIMESTAMP)`, childID)
	require.NoError(t, err)

	var linkCount int
	require.NoError(t, staged.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM subagent_links`).Scan(&linkCount))
	require.Equal(t, 1, linkCount)
	require.NoError(t, staged.Close())

	// Boot a real daemon on the staged database: Run applies every later migration.
	respond := func(_ string, msgs []llmwire.Message) *llmwire.Response {
		if hasUserContaining(msgs, "CHILD_STAGED") {
			return &llmwire.Response{Text: "staged child finished"}
		}

		return &llmwire.Response{Text: "parent done"}
	}
	h := newSubagentHarnessOnDB(t, dbPath, respond, nil)
	defer h.shutdown()

	require.NoError(t, h.mgr.Start(h.ctx))
	h.mgr.sweep(h.ctx)

	var timeoutCol int
	require.NoError(t, h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('subagent_links') WHERE name = 'timeout_sec'`).Scan(&timeoutCol))
	assert.Zero(t, timeoutCol, "the boot migration must drop the legacy column")

	link, err := h.links.GetLinkByTaskCallID(h.ctx, parent.ID, "tc_staged")
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, childID, link.ChildID,
		"the staged link still owns the call — no new child may spawn")

	// The child completes and delivers exactly once to the suspended parent.
	h.waitForDelivery(childID)
	h.mgr.waitIdle(parent.ID)

	final := h.parentMessages(parent.ID)
	require.NoError(t, llm.ValidateToolPairing(final))
	assert.Equal(t, 1, countAssistantToolCallsFor(final, tool.IDTask),
		"the task call is never re-issued")
	assert.Equal(t, 1, countToolResultsFor(final, tool.IDTask),
		"exactly one completion fills the staged call")
	require.Contains(t, lastToolResultContent(final, tool.IDTask), "staged child finished")

	var spawnCount int
	require.NoError(t, h.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sessions WHERE parent_id = ?`, parent.ID).Scan(&spawnCount))
	assert.Equal(t, 1, spawnCount, "recovery spawns no replacement child")

	link, err = h.links.GetLink(h.ctx, childID)
	require.NoError(t, err)
	require.NotNil(t, link)
	assert.Equal(t, subagent.OutcomeCompleted, link.Outcome)
}
