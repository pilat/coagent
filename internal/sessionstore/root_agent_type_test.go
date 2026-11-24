package sessionstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore/sessionstoretest"
)

// The sessions.agent_type schema default is 'general' — a subagent type. A root
// session must carry its own type in the row, not inherit that default.
func TestCreateRootPersistsTheRootAgentType(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)

	rec, _, err := store.CreateRoot(ctx, RootCreate{
		ProjectID: projectID, Model: "model-a", ReasoningLevel: "medium",
		Name: "test", WorkDir: "/project",
	})
	require.NoError(t, err)
	assert.Equal(t, "build", rec.AgentType, "the returned record names the agent type it wrote")

	var stored string
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT agent_type FROM sessions WHERE id = ?`, rec.ID).Scan(&stored))
	assert.Equal(t, "build", stored, "the row must not fall back to the subagent default")

	loaded, err := store.GetSession(ctx, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, "build", loaded.AgentType)
}

// Subagent rows keep naming their own type; the root default must not leak into
// the spawn path.
func TestCreateSubagentSessionKeepsItsOwnAgentType(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newTestStore(t)

	root := sessionstoretest.BareRoot(t, store.db, projectID, "model-a", "medium", nil)

	childID, err := createChild(ctx, store, projectID, root, root, "explore", "model-a", "medium")
	require.NoError(t, err)

	child, err := store.GetSession(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, "explore", child.AgentType)
}
