package managercontrol

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestLineage_EveryControllerFollowsAReplacement(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	factory, store, project, _ := newChatControl(t)
	root, _, err := store.CreateRoot(ctx, sessionstore.RootCreate{
		ProjectID: project, Model: "model", Name: "test", WorkDir: "/project",
		Attributes: map[string]any{},
	})
	require.NoError(t, err)
	alpha := factory.ForManager("alpha").(controllerapi.LineageController)
	next, err := alpha.Successor(ctx, root.ID)
	require.NoError(t, err)
	assert.Zero(t, next)
	replacement, _, err := store.ReplaceRoot(ctx, root.ID, "test", "/project")
	require.NoError(t, err)
	next, err = alpha.Successor(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, replacement.ID, next)
	next, err = alpha.Successor(ctx, replacement.ID)
	require.NoError(t, err)
	assert.Zero(t, next)
	next, err = factory.ForManager("beta").(controllerapi.LineageController).Successor(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, replacement.ID, next)
	_, err = store.MarkSessionKilledWithOutput(ctx, replacement.ID, 0)
	require.NoError(t, err)
	next, err = alpha.Successor(ctx, replacement.ID)
	require.NoError(t, err)
	assert.Zero(t, next)
}
