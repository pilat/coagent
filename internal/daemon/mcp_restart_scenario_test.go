package daemon

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
)

type mcpRestartHarness struct {
	*subagentHarness
	db       *sql.DB
	registry mcpstore.Store
	pool     mcp.Pool
}

func TestScenario_MCPDisablePersistsAcrossDaemonRestart(t *testing.T) {
	fake := newFakeMCPServer(t, "pong before restart", false)
	dbPath := filepath.Join(t.TempDir(), "mcp-restart.db")
	workDir := t.TempDir()
	respond := disableScenarioResponder(fake)

	first := newMCPRestartHarness(t, dbPath, workDir, respond)
	sessionID, err := first.mgr.Send(first.ctx, first.projectID, "register the fake server", "fake-model", nil)
	require.NoError(t, err)
	first.waitUntil("registration finishes", func() bool {
		return lastAssistantTextDTO(first.parentMessages(sessionID)) == "registered"
	})

	require.NoError(t, first.mgr.SendToSession(first.ctx, sessionID, "USE_IT now"))
	first.waitUntil("pooled call finishes", func() bool {
		return lastAssistantTextDTO(first.parentMessages(sessionID)) == "used before restart"
	})
	assert.Equal(t, 1, fake.count(t, "spawn"))

	require.NoError(t, first.mgr.SendToSession(first.ctx, sessionID, "DISABLE_IT now"))
	first.waitUntil("disable finishes", func() bool {
		return lastAssistantTextDTO(first.parentMessages(sessionID)) == "disabled"
	})
	defs, err := first.registry.ListForProject(first.ctx, first.projectID)
	require.NoError(t, err)
	assert.Empty(t, defs, "the disabled row is absent from the session's enabled projection")
	first.close()

	second := newMCPRestartHarness(t, dbPath, workDir, respond)
	require.NoError(t, second.mgr.Start(second.ctx))
	require.NoError(t, second.mgr.SendToSession(second.ctx, sessionID, "USE_AFTER_RESTART now"))
	second.waitUntil("post-restart run finishes", func() bool {
		return lastAssistantTextDTO(second.parentMessages(sessionID)) == "used after restart"
	})

	messages := second.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(messages))
	assert.Contains(t, toolResultForCallID(messages, "ping-after-restart"), "unknown tool",
		"a disabled registry row must not return in a fresh daemon activation")
	assert.Equal(t, 1, fake.count(t, "spawn"), "restart must not spawn stale MCP availability")
}

func disableScenarioResponder(fake *fakeMCPServer) func(string, []llmwire.Message) *llmwire.Response {
	return func(_ string, messages []llmwire.Message) *llmwire.Response {
		last := lastUserText(messages)
		switch {
		case strings.Contains(last, "USE_AFTER_RESTART"):
			if hasToolResultForCallID(messages, "ping-after-restart") {
				return &llmwire.Response{Text: "used after restart"}
			}
			return mcpPingCall("ping-after-restart")
		case strings.Contains(last, "USE_IT"):
			if hasToolResultForCallID(messages, "ping-before-restart") {
				return &llmwire.Response{Text: "used before restart"}
			}
			return mcpPingCall("ping-before-restart")
		case strings.Contains(last, "DISABLE_IT"):
			if hasToolResultFor(messages, tool.IDMCPDisable) {
				return &llmwire.Response{Text: "disabled"}
			}
			return mcpToolCall("disable-1", tool.IDMCPDisable, `{"name":"fake","scope":"project"}`)
		default:
			if hasToolResultFor(messages, tool.IDMCPAdd) {
				return &llmwire.Response{Text: "registered"}
			}
			return mcpToolCall("add-1", tool.IDMCPAdd, fake.addParams("fake", "project"))
		}
	}
}

func TestScenario_MCPRemoveEvictsPooledProcessBeforeTheNextRun(t *testing.T) {
	fake := newExitTrackingMCPServer(t, "pong before removal")
	dbPath := filepath.Join(t.TempDir(), "mcp-remove.db")
	workDir := t.TempDir()
	respond := removeScenarioResponder(t, fake)
	h := newMCPRestartHarness(t, dbPath, workDir, respond)

	sessionID, err := h.mgr.Send(h.ctx, h.projectID, "register the fake server", "fake-model", nil)
	require.NoError(t, err)
	h.waitUntil("registration finishes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "registered"
	})
	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "USE_IT now"))
	h.waitUntil("pooled call finishes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "used before remove"
	})
	assert.Equal(t, 1, fake.count(t, "spawn"))

	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "REMOVE_IT now"))
	h.waitUntil("removal finishes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "removed"
	})
	fake.waitForExit(t)
	require.NoError(t, h.mgr.SendToSession(h.ctx, sessionID, "USE_AFTER_REMOVE now"))
	h.waitUntil("post-removal run finishes", func() bool {
		return lastAssistantTextDTO(h.parentMessages(sessionID)) == "used after remove"
	})

	messages := h.parentMessages(sessionID)
	require.NoError(t, llm.ValidateToolPairing(messages))
	assert.Contains(t, toolResultForCallID(messages, "ping-after-remove"), "unknown tool",
		"a removed row must be absent from the next stack")
	assert.Equal(t, 1, fake.count(t, "spawn"), "removal must evict instead of leaving a stale pool entry")
}

func removeScenarioResponder(
	t *testing.T,
	fake *exitTrackingMCPServer,
) func(string, []llmwire.Message) *llmwire.Response {
	t.Helper()

	return func(_ string, messages []llmwire.Message) *llmwire.Response {
		last := lastUserText(messages)
		switch {
		case strings.Contains(last, "USE_AFTER_REMOVE"):
			if hasToolResultForCallID(messages, "ping-after-remove") {
				return &llmwire.Response{Text: "used after remove"}
			}

			return mcpPingCall("ping-after-remove")
		case strings.Contains(last, "USE_IT"):
			if hasToolResultForCallID(messages, "ping-before-remove") {
				return &llmwire.Response{Text: "used before remove"}
			}

			return mcpPingCall("ping-before-remove")
		case strings.Contains(last, "REMOVE_IT"):
			if hasToolResultFor(messages, tool.IDMCPRemove) {
				return &llmwire.Response{Text: "removed"}
			}

			return mcpToolCall("remove-1", tool.IDMCPRemove, `{"name":"fake","scope":"project"}`)
		default:
			if hasToolResultFor(messages, tool.IDMCPAdd) {
				return &llmwire.Response{Text: "registered"}
			}

			return mcpToolCall("add-1", tool.IDMCPAdd, exitTrackingParams(t, fake))
		}
	}
}

func newMCPRestartHarness(
	t *testing.T,
	dbPath string,
	workDir string,
	respond func(string, []llmwire.Message) *llmwire.Response,
) *mcpRestartHarness {
	t.Helper()
	ctx := context.Background()
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	store := NewStore(db)
	sessStore := sessionstore.NewStore(db)
	links := subagent.NewStore(db)
	schedStore := schedule.NewStore(db)
	registry := mcpstore.NewStore(db)
	pool := mcp.NewPool(nil)
	cfg := &config.Config{WorkDir: workDir, Model: "fake-model"}
	factory := session.NewFactoryWithOptions(
		cfg, nil, nil, sessStore, sessStore, nil, pool, registry, nil, nil,
		session.WithLLMClientFactory(func(_ *config.Config) (llm.Client, error) {
			return &scriptedLLM{respond: respond}, nil
		}),
	)
	mgr := newSvc(
		factory,
		store,
		sessStore,
		sessStore,
		links,
		subagent.NewTransactions(db),
		budget.New(sessStore),
		sessStore,
		schedule.NewService(schedStore),
		func() string { return "fake-model" },
	)
	mgr.mcpStore = registry
	mgr.mcpPool = pool
	projectID, err := store.GetOrCreateProject(ctx, workDir)
	require.NoError(t, err)

	h := &mcpRestartHarness{
		subagentHarness: &subagentHarness{
			t: t, mgr: mgr, sessStore: sessStore, links: links, schedStore: schedStore,
			projectID: projectID, ctx: ctx,
		},
		db:       db,
		registry: registry,
		pool:     pool,
	}
	t.Cleanup(h.close)

	return h
}

func (h *mcpRestartHarness) close() {
	h.mgr.Shutdown(5 * time.Second)
	h.pool.Stop()
	_ = h.db.Close()
}
