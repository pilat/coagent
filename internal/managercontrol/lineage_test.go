package managercontrol

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestLineage_SuccessorFollowsOnlyAnOwnedReplacement(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	factory, store, project := newChatControl(t)
	root, _, err := store.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: project, Model: "model", Name: "test", WorkDir: "/project",
		Attributes: map[string]any{controllerapi.SessionAttributeManagerID: "alpha"},
	})
	require.NoError(t, err)
	alpha := factory.ForManager("alpha").(controllerapi.LineageController)
	next, err := alpha.Successor(ctx, root.ID)
	require.NoError(t, err)
	assert.Zero(t, next)
	replacement, _, err := store.ReplaceManagerRoot(ctx, root.ID, "test", "/project")
	require.NoError(t, err)
	next, err = alpha.Successor(ctx, root.ID)
	require.NoError(t, err)
	assert.Equal(t, replacement.ID, next)
	next, err = alpha.Successor(ctx, replacement.ID)
	require.NoError(t, err)
	assert.Zero(t, next)
	_, err = factory.ForManager("beta").(controllerapi.LineageController).Successor(ctx, root.ID)
	require.Error(t, err)
	_, err = store.MarkSessionKilledWithOutput(ctx, replacement.ID, 0)
	require.NoError(t, err)
	next, err = alpha.Successor(ctx, replacement.ID)
	require.NoError(t, err)
	assert.Zero(t, next)
}
