package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/managercontrol"
	"github.com/pilat/coagent/internal/managerdiscovery"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

func newTestController(
	svc *svc,
	cfg *config.Config,
	cache loader.MarketplaceCache,
	_ schedule.Service,
) controllerapi.ManagerControllerFactory {
	var outputs *sessionstore.Store
	if svc != nil {
		outputs, _ = svc.store.(*sessionstore.Store)
	}

	return managercontrol.New(
		svc,
		outputs,
		managerdiscovery.New(outputs, cfg, cache),
		svc.progress,
		svc.bus,
		cfg,
		cache,
	)
}

func TestControllerManagerSubscriptionIsExactAcrossRestart(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "routing.db")
	firstDB, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	require.NoError(t, migrate.Run(ctx, firstDB, dbPath))
	firstStore := sessionstore.NewStore(firstDB)
	firstSessions := sessionstore.NewStore(firstDB)
	projectID := testProject(t, firstStore, "/tmp/controller-manager-restart")
	record, err := firstSessions.CreateSession(ctx, projectID, "model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: "manager-7",
	})
	require.NoError(t, err)
	require.NoError(t, firstDB.Close())

	secondDB, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = secondDB.Close() })
	secondSessions := sessionstore.NewStore(secondDB)
	mgr, _ := newScenarioDaemon(
		context.Background(),
		scriptedBuildInput(
			t,
			&config.Config{Model: "fake-model"},
			secondSessions,
			nil,
			func(*config.Config) (llm.Client, error) { return &scriptedLLM{respond: trivialRespond}, nil },
		),
		secondSessions,
		subagent.NewStore(secondDB, secondSessions),
		nil,
		nil,
		nil,
		secondDB,
	)
	controllers := newTestController(mgr, &config.Config{}, nil, nil)
	subscriptions := make(map[string]<-chan controllerapi.SessionNotification, 10)
	for i := range 10 {
		managerID := fmt.Sprintf("manager-%d", i)
		subscriptions[managerID] = controllers.ForManager(managerID).Subscribe()
	}

	mgr.NotifySession(record.ID, sessionevent.Notification{
		Type: sessionevent.NotifyMessage, Message: "after restart",
	})

	for managerID, subscription := range subscriptions {
		if managerID == "manager-7" {
			notification := requireManagerNotification(t, subscription)
			assert.Equal(t, "after restart", notification.Notification.Message)
			continue
		}

		requireNoManagerNotification(t, subscription)
	}
}
