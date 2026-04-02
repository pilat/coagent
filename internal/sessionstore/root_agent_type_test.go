package sessionstore

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The sessions.agent_type schema default is 'general' — a subagent type. A root
// session must carry its own type in the row, not inherit that default.
func TestCreateSessionPersistsTheRootAgentType(t *testing.T) {
	ctx := context.Background()
	store, db, projectID := newTestStore(t)

	rec, err := store.CreateSession(ctx, projectID, "model-a", "medium", nil)
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

	root, err := store.CreateSession(ctx, projectID, "model-a", "medium", nil)
	require.NoError(t, err)

	childID, err := store.CreateSubagentSession(ctx, projectID, root.ID, root.ID, "explore", "model-a", "medium")
	require.NoError(t, err)

	child, err := store.GetSession(ctx, childID)
	require.NoError(t, err)
	assert.Equal(t, "explore", child.AgentType)
}
