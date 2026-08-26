package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
)

func TestManagerBoundControllerRejectsEveryForeignSessionOperation(t *testing.T) {
	t.Parallel()

	mgr, _, store := newTestManager(t)
	ctx := context.Background()
	projectID := testProject(t, store, "/tmp/controller-owner-operations")
	alphaRecord, err := mgr.sessionStore.CreateSession(ctx, projectID, "model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: "alpha",
	})
	require.NoError(t, err)
	betaRecord, err := mgr.sessionStore.CreateSession(ctx, projectID, "model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: "beta",
	})
	require.NoError(t, err)
	controller := NewController(mgr, &config.Config{}, nil, nil).ForManager("alpha")

	sessions, err := controller.ListSessions(ctx)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	assert.Equal(t, alphaRecord.ID, sessions[0].ID)

	operations := []struct {
		name string
		call func() error
	}{
		{name: "send", call: func() error {
			return controller.SendSessionMessage(ctx, controllerapi.SessionMessageData{
				SessionID: betaRecord.ID, Message: "foreign",
			})
		}},
		{name: "set model", call: func() error {
			return controller.SetSessionModel(ctx, controllerapi.SessionSetModelData{
				SessionID: betaRecord.ID, Model: "other",
			})
		}},
		{name: "set attributes", call: func() error {
			return controller.SetSessionAttributes(ctx, controllerapi.SessionSetAttributesData{
				SessionID: betaRecord.ID, Attributes: map[string]any{"topic": 9},
			})
		}},
		{name: "list skills", call: func() error {
			_, listErr := controller.ListSkills(ctx, controllerapi.ConfigSkillsData{SessionID: betaRecord.ID})
			return listErr
		}},
	}

	for _, operation := range operations {
		t.Run(operation.name, func(t *testing.T) {
			require.ErrorContains(t, operation.call(), "belongs to another manager")
		})
	}

	stored, err := mgr.sessionStore.GetSession(ctx, betaRecord.ID)
	require.NoError(t, err)
	assert.Nil(t, stored.KilledAt)
	assert.Equal(t, "model", stored.Model)
	assert.NotContains(t, stored.Attributes, "topic")
}

func TestManagerBoundControllerClaimsOnlyCanonicalLegacyCLIChat(t *testing.T) {
	t.Parallel()

	mgr, _, store := newTestManager(t)
	ctx := context.Background()
	root := t.TempDir()
	workDir := filepath.Join(root, controllerapi.CoagentSystemProjectDir)
	mgr.systemProject = workDir
	projectID, err := store.GetOrCreateSystemProject(
		ctx, workDir, controllerapi.CoagentSystemProjectName,
	)
	require.NoError(t, err)
	legacy, err := mgr.sessionStore.CreateSession(ctx, projectID, "model", "", map[string]any{
		"channel": controllerapi.BuiltinCLIManagerID,
	})
	require.NoError(t, err)
	factory := NewController(mgr, &config.Config{UnifiedConfig: &config.UnifiedConfig{
		ProjectsRoot: root,
	}}, nil, nil)
	cliController := factory.ForManager(controllerapi.BuiltinCLIManagerID)
	telegramController := factory.ForManager("telegram-main")

	cliSessions, err := cliController.ListSessions(ctx)
	require.NoError(t, err)
	require.Len(t, cliSessions, 1)
	telegramSessions, err := telegramController.ListSessions(ctx)
	require.NoError(t, err)
	assert.Empty(t, telegramSessions)

	require.Error(t, telegramController.SetSessionAttributes(ctx, controllerapi.SessionSetAttributesData{
		SessionID: legacy.ID, Attributes: legacy.Attributes,
	}))
	require.NoError(t, cliController.SetSessionAttributes(ctx, controllerapi.SessionSetAttributesData{
		SessionID: legacy.ID, Attributes: legacy.Attributes,
	}))
	claimed, err := mgr.sessionStore.GetSession(ctx, legacy.ID)
	require.NoError(t, err)
	assert.Equal(t, controllerapi.BuiltinCLIManagerID, claimed.Attributes[controllerapi.SessionAttributeManagerID])
}
