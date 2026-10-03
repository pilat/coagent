package schedule

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

type waitGuardTool struct{ calls int }

func (t *waitGuardTool) ID() string                  { return tool.IDSleep }
func (t *waitGuardTool) ParallelSafe() bool          { return false }
func (t *waitGuardTool) Description() string         { return "sleep" }
func (t *waitGuardTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (t *waitGuardTool) Execute(context.Context, json.RawMessage) (*tool.Result, error) {
	t.calls++
	return &tool.Result{Output: "slept"}, nil
}

func TestBackgroundWaitGuardRejectsSleepUntilCompletionDelivered(t *testing.T) {
	inner := &waitGuardTool{}
	dbPath := filepath.Join(t.TempDir(), "guard.db")
	db, err := migrate.OpenDB(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(t.Context(), db, dbPath))
	sessions := sessionstore.NewStore(db)
	projectID, err := sessions.GetOrCreateProject(t.Context(), t.TempDir())
	require.NoError(t, err)
	parent, err := sessions.CreateSession(t.Context(), projectID, "model", "", nil)
	require.NoError(t, err)
	children := subagent.NewStore(db, sessions)
	childID, err := children.Create(t.Context(), subagent.Create{
		ProjectID: projectID, ParentID: parent.ID, RootID: parent.ID, Model: "model",
		TaskCallID: "task-1", State: subagent.StateRunning,
	})
	require.NoError(t, err)
	guard := &backgroundWaitGuard{inner: inner, sessions: sessions, sessionID: parent.ID}

	_, err = guard.Execute(t.Context(), nil)
	require.Error(t, err)
	require.ErrorContains(t, err, "result arrives automatically in a later turn")
	require.ErrorContains(t, err, "end the response")
	assert.Zero(t, inner.calls, "the sleep side effect must not be staged")

	_, err = sessions.Commit(t.Context(), sessionstore.Commit{
		SessionID: childID,
		Messages:  []*transcript.Message{{Role: "assistant", Content: "done", FinishType: "stop"}},
	})
	require.NoError(t, err)
	link, err := children.Finalize(t.Context(), childID, false)
	require.NoError(t, err)
	delivered, err := subagent.NewStore(db, sessions).DeliverBackgroundCompletion(t.Context(), *link, 1)
	require.NoError(t, err)
	require.True(t, delivered)
	result, err := guard.Execute(t.Context(), nil)
	require.NoError(t, err)
	assert.Equal(t, "slept", result.Output)
	assert.Equal(t, 1, inner.calls)
}
