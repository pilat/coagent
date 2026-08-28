package cli

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/daemon"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
)

type delayedCLIHarness struct {
	controller        controllerapi.Controller
	foreignController controllerapi.Controller
	sessions          sessionstore.Store
	service           daemon.Service
	db                *sql.DB
	workDir           string
	socket            string
}

// A real session may finish before the local chat manager is available. Its
// outbox rows must survive that gap and reach the first attached terminal.
func TestHarnessScenario_DelayedCLIManagerDrainsRealSessionOutput(t *testing.T) {
	h := newDelayedCLIHarness(t)
	sessionID := h.produceBeforeManager(t)
	h.waitForBacklog(t)

	terminal, _ := h.startManagerAndDial(t)

	opened := openChat(t, terminal)
	require.Equal(t, sessionID, opened.SessionID)
	assert.Equal(t, "delayed cli answer", waitForDelayedCLIMessage(t, terminal, sessionID).Message)
	require.Eventually(t, func() bool {
		status, statusErr := h.sessions.OutputQueueStatus(t.Context(), controllerapi.BuiltinCLIManagerID)
		return statusErr == nil && status.Pending == 0
	}, 10*time.Second, 10*time.Millisecond, "the attached terminal must acknowledge the durable backlog")
}

func newDelayedCLIHarness(t *testing.T) *delayedCLIHarness {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "coagent.db")
	db, err := migrate.OpenDB(t.Context(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(t.Context(), db, dbPath))

	projects := daemon.NewStore(db)
	sessions := sessionstore.NewStore(db)
	projectsRoot := filepath.Join(root, "projects")
	cfg := &config.Config{
		Model: "fake-model", WorkDir: root,
		UnifiedConfig: &config.UnifiedConfig{ProjectsRoot: projectsRoot},
	}
	factory := session.NewFactoryWithOptions(
		cfg, nil, nil, sessions, nil, nil, nil, nil, nil,
		session.WithLLMClientFactory(func(*config.Config) (llm.Client, error) {
			return delayedCLIClient{}, nil
		}),
	)
	service := daemon.New(
		factory, projects, sessions, sessions, daemon.NewLinkStore(db),
		schedule.NewService(schedule.NewStore(db)), cfg, nil, nil, nil,
	)
	t.Cleanup(func() { service.Shutdown(3 * time.Second) })
	controllers := daemon.NewController(service, cfg, nil, nil)
	controller := controllers.ForManager(controllerapi.BuiltinCLIManagerID)

	return &delayedCLIHarness{
		controller: controller, foreignController: controllers.ForManager("telegram-main"),
		sessions: sessions, service: service, db: db, workDir: root, socket: scenarioSocket(t),
	}
}

func (h *delayedCLIHarness) startManagerAndDial(t *testing.T) (*ctl.Client, *Manager) {
	t.Helper()
	server, err := ctl.NewServer(t.Context(), h.socket, "test", ctl.Deps{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })
	manager := New(h.controller, server, "fake-model", newFakeSecrets())
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	go func() { _ = server.Serve(context.Background()) }()

	terminal, err := ctl.Dial(t.Context(), h.socket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = terminal.Close() })

	return terminal, manager
}

func (h *delayedCLIHarness) produceBeforeManager(t *testing.T) int64 {
	t.Helper()
	project, err := h.controller.CreateProject(t.Context(), controllerapi.ProjectCreateData{
		Name: ProjectName, System: true,
	})
	require.NoError(t, err)
	require.NotZero(t, project.ID)

	sessionID, err := h.controller.CreateSession(t.Context(), controllerapi.SessionCreateData{
		SystemProject: ProjectName, WorkDir: project.Path,
		Prompt: "finish before local chat starts", Model: "fake-model",
	})
	require.NoError(t, err)

	return sessionID
}

func (h *delayedCLIHarness) waitForBacklog(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool {
		status, err := h.sessions.OutputQueueStatus(t.Context(), controllerapi.BuiltinCLIManagerID)
		return err == nil && status.Pending >= 2
	}, 10*time.Second, 10*time.Millisecond, "the real session must commit lifecycle and terminal output before manager start")
}

func waitForDelayedCLIMessage(t *testing.T, terminal *ctl.Client, sessionID int64) Event {
	t.Helper()
	for {
		output := waitForDelayedCLIEvent(t, terminal)
		if output.SessionID == sessionID && output.Type == "message" {
			return output
		}
	}
}

func waitForDelayedCLIEvent(t *testing.T, terminal *ctl.Client) Event {
	t.Helper()
	select {
	case event := <-terminal.Notifications():
		require.Equal(t, EventMethod, event.Method)

		var output Event
		require.NoError(t, json.Unmarshal(event.Params, &output))

		return output
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for CLI output")
	}

	return Event{}
}

type delayedCLIClient struct{}

func (delayedCLIClient) Chat(
	context.Context,
	string,
	[]llmwire.Message,
	[]llmwire.ToolSchema,
	...llmwire.ChatOption,
) (*llmwire.Response, error) {
	return &llmwire.Response{Text: "delayed cli answer"}, nil
}

func (delayedCLIClient) Model() string             { return "fake-model" }
func (delayedCLIClient) APIKey() string            { return "" }
func (delayedCLIClient) Close() error              { return nil }
func (delayedCLIClient) Provider() string          { return "fake" }
func (delayedCLIClient) ContextWindow() int        { return 200000 }
func (delayedCLIClient) SetReasoningLevel(string)  {}
func (delayedCLIClient) GetReasoningLevel() string { return "medium" }
func (delayedCLIClient) SetSessionID(string)       {}
