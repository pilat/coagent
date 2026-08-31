package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/daemon"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

type cliOwnershipHarness struct {
	svc               daemon.Service
	sessions          sessionstore.Store
	projectID         int64
	foreignController controllerapi.Controller
	socket            string
}

// A foreign event must not cross the durable owner boundary into the local chat renderer.
func TestHarnessScenario_DurableManagerOwnershipReachesOnlyTheCLIRenderer(t *testing.T) {
	h := newCLIOwnershipHarness(t)
	owned, foreign := h.createSessions(t)
	terminal := h.dial(t)
	require.Equal(t, owned, openChat(t, terminal).SessionID)

	foreignEvents := h.foreignController.Subscribe()
	t.Cleanup(func() { h.foreignController.Unsubscribe(foreignEvents) })
	h.publish(t, owned, "✅ local owner answer")
	h.publish(t, foreign, "❌ telegram answer")
	h.publish(t, owned, "✅ local owner barrier")

	assertCLIEvents(t, terminal, owned)
	assertForeignControllerEvent(t, foreignEvents, foreign)
}

func newCLIOwnershipHarness(t *testing.T) *cliOwnershipHarness {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	dbPath := filepath.Join(root, "coagent.db")
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	projects := daemon.NewStore(db)
	sessions := sessionstore.NewStore(db)
	cfg := &config.Config{UnifiedConfig: &config.UnifiedConfig{ProjectsRoot: filepath.Join(root, "projects")}}
	svc := daemon.New(
		nil, projects, sessions, sessions, sessions, sessions, sessions, sessions, sessions,
		subagent.NewStore(db), subagent.NewTransactions(db),
		budget.New(sessions), sessions, nil, cfg, nil, nil, nil,
	)
	controllers := daemon.NewController(svc, cfg, nil, nil)
	cliController := controllers.ForManager(controllerapi.BuiltinCLIManagerID)
	socket := scenarioSocket(t)
	server, err := ctl.NewServer(ctx, socket, "test", ctl.Deps{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })
	manager := New(cliController, server, "test-model", newFakeSecrets())
	require.NoError(t, manager.Start(ctx))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	go func() { _ = server.Serve(ctx) }()

	projectID, err := projects.GetOrCreateSystemProject(
		ctx,
		filepath.Join(root, "projects", controllerapi.CoagentSystemProjectDir),
		controllerapi.CoagentSystemProjectName,
	)
	require.NoError(t, err)

	return &cliOwnershipHarness{
		svc: svc, sessions: sessions, projectID: projectID, socket: socket,
		foreignController: controllers.ForManager("telegram-main"),
	}
}

func (h *cliOwnershipHarness) createSessions(t *testing.T) (int64, int64) {
	t.Helper()
	owned, err := h.sessions.CreateSession(context.Background(), h.projectID, "test-model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: controllerapi.BuiltinCLIManagerID,
	})
	require.NoError(t, err)
	foreign, err := h.sessions.CreateSession(context.Background(), h.projectID, "test-model", "", map[string]any{
		controllerapi.SessionAttributeManagerID: "telegram-main",
	})
	require.NoError(t, err)

	return owned.ID, foreign.ID
}

func (h *cliOwnershipHarness) dial(t *testing.T) *ctl.Client {
	t.Helper()
	terminal, err := ctl.Dial(context.Background(), h.socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = terminal.Close() })

	return terminal
}

func (h *cliOwnershipHarness) publish(t *testing.T, sessionID int64, message string) {
	t.Helper()
	_, err := h.sessions.EnqueueOutput(context.Background(), sessionstore.OutputDraft{
		SessionID: sessionID, Type: sessionstore.OutputMessagePersistent, Content: message,
	})
	require.NoError(t, err)
	h.svc.NotifySession(sessionID, sessionevent.Notification{Type: sessionevent.NotifyMessage, Message: message})
}

func assertCLIEvents(t *testing.T, terminal *ctl.Client, sessionID int64) {
	t.Helper()
	for _, message := range []string{"✅ local owner answer", "✅ local owner barrier"} {
		event := waitForMessageEvent(t, terminal)
		assert.Equal(t, sessionID, event.SessionID)
		assert.Equal(t, message, event.Message)
	}
}

func waitForMessageEvent(t *testing.T, terminal *ctl.Client) Event {
	t.Helper()
	for {
		event := waitForEvent(t, terminal)
		if event.Type == "message" {
			return event
		}
	}
}

func assertForeignControllerEvent(
	t *testing.T,
	subscription <-chan controllerapi.SessionNotification,
	sessionID int64,
) {
	t.Helper()
	select {
	case notification := <-subscription:
		assert.Equal(t, sessionID, notification.SessionID)
		assert.Equal(t, "❌ telegram answer", notification.Notification.Message)
	case <-time.After(3 * time.Second):
		t.Fatal("foreign manager did not receive its owned notification")
	}
}
