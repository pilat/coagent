package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/budget"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

// activationSchemas stores each request's inventory separately. A union would
// hide a stale registry that survived into a later activation.
type activationSchemas struct {
	mu   sync.Mutex
	byID map[int64][][]string
}

func (r *activationSchemas) record(sessionID int64, schemas []llmwire.ToolSchema) {
	r.mu.Lock()
	defer r.mu.Unlock()

	ids := make([]string, 0, len(schemas))
	for _, schema := range schemas {
		ids = append(ids, schema.Name)
	}
	r.byID[sessionID] = append(r.byID[sessionID], ids)
}

func (r *activationSchemas) first(t *testing.T, sessionID int64) []string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.byID[sessionID], "no LLM request for session %d", sessionID)

	return append([]string(nil), r.byID[sessionID][0]...)
}

func (r *activationSchemas) last(t *testing.T, sessionID int64) []string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.byID[sessionID], "no LLM request for session %d", sessionID)

	requests := r.byID[sessionID]
	return append([]string(nil), requests[len(requests)-1]...)
}

type registryPromptLLM struct {
	scriptedLLM
	recorder *activationSchemas
	prompts  *promptRecorder

	mu        sync.Mutex
	sessionID int64
}

type registryPromptDeps struct {
	ctx          context.Context
	store        Store
	sessionStore sessionstore.Store
	links        subagent.Store
	subagents    subagent.Transactions
	schedules    schedule.Store
	mcpRegistry  mcpstore.Store
	mcpPool      mcp.Pool
}

func (c *registryPromptLLM) SetSessionID(id string) {
	if index := strings.LastIndex(id, ":"); index >= 0 {
		id = id[index+1:]
	}

	parsed, _ := strconv.ParseInt(id, 10, 64)
	c.mu.Lock()
	c.sessionID = parsed
	c.mu.Unlock()
}

func (c *registryPromptLLM) Chat(
	ctx context.Context,
	system string,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	opts ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	c.mu.Lock()
	sessionID := c.sessionID
	c.mu.Unlock()

	c.prompts.record(strconv.FormatInt(sessionID, 10), system)
	c.recorder.record(sessionID, tools)

	return c.scriptedLLM.Chat(ctx, system, messages, tools, opts...)
}

func (r *promptRecorder) last(t *testing.T, role string) string {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	require.NotEmpty(t, r.byRole[role], "no %s request was recorded", role)

	requests := r.byRole[role]
	return requests[len(requests)-1]
}

func newRegistryPromptDeps(t *testing.T) registryPromptDeps {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "registry-prompt.db")
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	mcpPool := mcp.NewPool(nil)
	t.Cleanup(mcpPool.Stop)
	return registryPromptDeps{
		ctx: ctx, store: NewStore(db), sessionStore: sessionstore.NewStore(db),
		links: subagent.NewStore(db), subagents: subagent.NewTransactions(db), schedules: schedule.NewStore(db),
		mcpRegistry: mcpstore.NewStore(db), mcpPool: mcpPool,
	}
}

// newRegistryPromptHarness is the composition-root MCP wiring with a system
// project and a scripted LLM that records each live registry projection.
func newRegistryPromptHarness(
	t *testing.T,
	respond func(string, []llmwire.Message) *llmwire.Response,
) (*subagentHarness, *activationSchemas, *promptRecorder, mcpstore.Store) {
	t.Helper()

	deps := newRegistryPromptDeps(t)

	workDir := filepath.Join(t.TempDir(), controllerapi.CoagentSystemProjectDir)
	require.NoError(t, os.MkdirAll(workDir, 0o755))
	cfg := &config.Config{WorkDir: workDir, Model: "fake-model"}
	recorder := &activationSchemas{byID: make(map[int64][][]string)}
	prompts := newPromptRecorder()

	factory := newRegistryPromptFactory(cfg, deps, respond, recorder, prompts)
	mgr := newRegistryPromptManager(deps, factory, workDir, t)
	projectID, err := deps.store.GetOrCreateSystemProject(deps.ctx, workDir, controllerapi.CoagentSystemProjectName)
	require.NoError(t, err)

	return &subagentHarness{
		t: t, mgr: mgr, sessStore: deps.sessionStore, links: deps.links, schedStore: deps.schedules,
		projectID: projectID, ctx: deps.ctx,
	}, recorder, prompts, deps.mcpRegistry
}

func newRegistryPromptFactory(
	cfg *config.Config,
	deps registryPromptDeps,
	respond func(string, []llmwire.Message) *llmwire.Response,
	recorder *activationSchemas,
	prompts *promptRecorder,
) session.Factory {
	return session.NewFactoryWithOptions(
		cfg, nil, nil, deps.sessionStore, nil, deps.mcpPool, deps.mcpRegistry, nil, nil,
		session.WithLLMClientFactory(func(_ *config.Config) (llm.Client, error) {
			return &registryPromptLLM{
				scriptedLLM: scriptedLLM{respond: respond}, recorder: recorder, prompts: prompts,
			}, nil
		}),
	)
}

func newRegistryPromptManager(
	deps registryPromptDeps,
	factory session.Factory,
	workDir string,
	t *testing.T,
) *svc {
	mgr := newSvc(
		factory, deps.store, deps.sessionStore, deps.sessionStore, deps.links, deps.subagents,
		budget.New(deps.sessionStore), schedule.NewService(deps.schedules),
		func() string { return "fake-model" },
	)
	mgr.systemProject = workDir
	mgr.mcpStore = deps.mcpRegistry
	mgr.mcpPool = deps.mcpPool
	mgr.applier = configapply.New(newTestConfigOps(t, t.TempDir()), func() {})

	return mgr
}
