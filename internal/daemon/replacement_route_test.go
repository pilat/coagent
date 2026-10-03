package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
)

func TestSendSessionMessageResolvedFollowsOwnedReplacement(t *testing.T) {
	ctx := context.Background()
	mgr, _, store := newTestManager(t)
	projectID, err := store.GetOrCreateProject(ctx, t.TempDir())
	require.NoError(t, err)
	old, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: "cli",
	})
	require.NoError(t, err)
	newID, err := mgr.clear(ctx, lifecycleInput(ctx, t, mgr, old.ID, "/clear"))
	require.NoError(t, err)

	controller := newTestController(mgr, &config.Config{}, nil, nil).ForManager("cli")
	router, ok := controller.(controllerapi.SessionMessageRouter)
	require.True(t, ok)
	acceptedID, err := router.SendSessionMessageResolved(ctx, controllerapi.SessionMessageData{
		SessionID: old.ID, Message: "racing clear",
	})
	require.NoError(t, err)
	assert.Equal(t, newID, acceptedID)
}
