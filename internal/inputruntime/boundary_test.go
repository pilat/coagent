package inputruntime

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

type activationBoundary interface {
	session.InputBoundary
	AcceptActivated(
		context.Context,
		session.PendingInput,
		string,
		[]session.PendingToolCall,
		tool.ActivationGrant,
	) (bool, bool, error)
	PendingActivation(context.Context) (*tool.ActivationGrant, error)
}

type commandBoundary interface {
	session.InputBoundary
	HandleWithOutput(context.Context, session.PendingInput, string, string) error
}

func TestBoundaryPromotesInputWithActivation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store, sessionID := newBoundaryStore(t, map[string]any{
		controllerapi.SessionAttributeManagerID: "manager-test",
	})
	queued, err := store.EnqueueInput(ctx, sessionID, sessionstore.InputSourceUser, "/budget set 1h")
	require.NoError(t, err)

	input := pendingInput(queued)
	boundary := New(store, nil).Boundary(sessionID, nil, nil, nil)
	activated, ok := boundary.(activationBoundary)
	require.True(t, ok)

	accepted, blocked, err := activated.AcceptActivated(ctx, input, "prepared", nil, tool.ActivationGrant{
		ToolID: "set_budget", Command: "/budget",
	})
	require.NoError(t, err)
	assert.True(t, accepted)
	assert.False(t, blocked)

	grant, err := activated.PendingActivation(ctx)
	require.NoError(t, err)
	require.NotNil(t, grant)
	assert.Equal(t, queued.ID, grant.InputID)
	assert.Equal(t, "set_budget", grant.ToolID)

	messages, err := store.LoadActiveMessages(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	assert.Equal(t, "prepared", messages[0].Content)
}

func TestBoundaryHandlesOwnedCommandWithOutputAtomically(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	attrs := map[string]any{controllerapi.SessionAttributeManagerID: "manager-test"}
	store, sessionID := newBoundaryStore(t, attrs)
	require.NoError(t, store.BindManager(ctx, "manager-test", "test", map[string]any{"account": "test"}))

	queued, err := store.EnqueueInput(ctx, sessionID, sessionstore.InputSourceUser, "/status")
	require.NoError(t, err)
	boundary := New(store, nil).Boundary(sessionID, nil, nil, nil)
	commands, ok := boundary.(commandBoundary)
	require.True(t, ok)

	require.NoError(t, commands.HandleWithOutput(ctx, pendingInput(queued), "status command", "ready"))

	status, err := store.OutputQueueStatus(ctx, "manager-test")
	require.NoError(t, err)
	assert.Equal(t, 1, status.Pending)

	_, err = store.PeekPending(ctx, sessionID)
	require.ErrorIs(t, err, sessionstore.ErrNoPendingInput)
}

func newBoundaryStore(t *testing.T, attrs map[string]any) (sessionstore.Store, int64) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "session.db")
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	projectID := insertBoundaryProject(t, db)
	store := sessionstore.NewStore(db)
	record, err := store.CreateSession(ctx, projectID, "model", "", attrs)
	require.NoError(t, err)

	return store, record.ID
}

func insertBoundaryProject(t *testing.T, db *sql.DB) int64 {
	t.Helper()

	result, err := db.ExecContext(
		context.Background(), `INSERT INTO projects (work_dir, name) VALUES (?, ?)`, t.TempDir(), "test",
	)
	require.NoError(t, err)

	projectID, err := result.LastInsertId()
	require.NoError(t, err)

	return projectID
}

func pendingInput(input *sessionstore.InboxInput) session.PendingInput {
	return session.PendingInput{
		ID: input.ID, Content: input.RawContent,
		Attributes: input.Attributes, ReceivedAt: input.ReceivedAt,
	}
}
