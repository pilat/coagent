package daemon

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
)

func TestCreateSession_PersistsOnlyTheBoundManagerOwner(t *testing.T) {
	ctx := context.Background()
	mgr, _, _ := newTestManager(t)
	ctrl := newTestController(mgr, &config.Config{}, nil, nil).ForManager("alpha")
	workDir := t.TempDir()

	ownedID, err := ctrl.CreateSession(ctx, controllerapi.SessionCreateData{
		WorkDir: workDir,
		Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "spoofed",
			"channel":                               "test",
		},
	})
	require.NoError(t, err)
	owned, err := mgr.store.GetSession(ctx, ownedID)
	require.NoError(t, err)
	assert.Equal(t, "alpha", owned.Attributes[controllerapi.SessionAttributeManagerID])
	assert.Equal(t, "test", owned.Attributes["channel"])

	secondID, err := ctrl.CreateSession(ctx, controllerapi.SessionCreateData{
		WorkDir: workDir,
		Attributes: map[string]any{
			controllerapi.SessionAttributeManagerID: "spoofed",
		},
	})
	require.NoError(t, err)
	second, err := mgr.store.GetSession(ctx, secondID)
	require.NoError(t, err)
	assert.Equal(t, "alpha", second.Attributes[controllerapi.SessionAttributeManagerID])
}
