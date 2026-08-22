package daemon

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/migrate"
)

type registryModelCommand string

const (
	registryAdd     registryModelCommand = "add"
	registryRebuild registryModelCommand = "rebuild"
	registryDisable registryModelCommand = "disable"
	registryRelease registryModelCommand = "release"
	registryEnable  registryModelCommand = "enable"
	registryRemove  registryModelCommand = "remove"
	registryRestart registryModelCommand = "restart"
)

type registryReference struct {
	registered bool
	enabled    bool
	visible    bool
}

type registryModelHarness struct {
	db        *sql.DB
	store     mcpstore.Store
	projectID int64
	workDir   string
	fake      *exitTrackingMCPServer
	pool      mcp.Pool
	service   mcp.Service
	reference registryReference
}

func TestHarnessModel_MCPRegistryProjectionAndPoolProtocol(t *testing.T) {
	h := newRegistryModelHarness(t)
	t.Cleanup(h.close)

	h.apply(t, registryAdd)
	h.assertVisible(t, false, "add is deferred until a stack rebuild")
	h.apply(t, registryRebuild)
	h.assertVisible(t, true, "rebuild acquires the enabled registry row")

	h.apply(t, registryDisable)
	h.assertVisible(t, true, "disable does not interrupt the current stack")
	h.apply(t, registryRelease)
	h.fake.waitForExit(t)
	h.assertVisible(t, false, "release closes the evicted disabled process")
	h.apply(t, registryRebuild)
	h.assertVisible(t, false, "disabled rows are absent from the next stack")

	h.apply(t, registryRestart)
	h.assertVisible(t, false, "restart does not resurrect disabled availability")
	h.apply(t, registryEnable)
	h.assertVisible(t, false, "enable is deferred until a later rebuild")
	h.apply(t, registryRebuild)
	h.assertVisible(t, true, "enabled availability appears on the next rebuild")

	h.apply(t, registryRemove)
	h.assertVisible(t, true, "remove retires but does not interrupt the current stack")
	h.apply(t, registryRelease)
	h.fake.waitForExitCount(t, 2)
	h.assertVisible(t, false, "release closes the evicted removed process")
	h.apply(t, registryRebuild)
	h.assertVisible(t, false, "removed availability cannot return")
	assert.Equal(t, 2, h.fake.count(t, "spawn"), "only the two enabled rebuilds spawn a process")
}

func newRegistryModelHarness(t *testing.T) *registryModelHarness {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "mcp-registry-model.db")
	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	workDir := t.TempDir()
	projectStore := NewStore(db)
	projectID, err := projectStore.GetOrCreateProject(ctx, workDir)
	require.NoError(t, err)

	fake := newExitTrackingMCPServer(t, "pong from model")
	return &registryModelHarness{
		db:        db,
		store:     mcpstore.NewStore(db),
		projectID: projectID,
		workDir:   workDir,
		fake:      fake,
		pool:      mcp.NewPool(nil),
	}
}

func (h *registryModelHarness) apply(t *testing.T, command registryModelCommand) {
	t.Helper()
	ctx := context.Background()

	switch command {
	case registryAdd:
		err := h.store.Add(ctx, &h.projectID, mcpstore.ServerDef{
			Name: "fake", Command: h.fake.path, Args: h.fake.args(), Enabled: true,
		})
		require.NoError(t, err)
		h.reference.registered = true
		h.reference.enabled = true
	case registryRebuild:
		h.rebuild(t)
	case registryDisable:
		require.NoError(t, h.store.SetEnabled(ctx, &h.projectID, "fake", false))
		h.pool.Evict("fake")
		h.reference.enabled = false
	case registryRelease:
		if h.service != nil {
			h.service.Stop()
			h.service = nil
		}
		h.reference.visible = false
	case registryEnable:
		require.NoError(t, h.store.SetEnabled(ctx, &h.projectID, "fake", true))
		h.reference.enabled = true
	case registryRemove:
		require.NoError(t, h.store.Remove(ctx, &h.projectID, "fake"))
		h.pool.Evict("fake")
		h.reference.registered = false
		h.reference.enabled = false
	case registryRestart:
		if h.service != nil {
			h.service.Stop()
			h.service = nil
		}
		h.pool.Stop()
		h.pool = mcp.NewPool(nil)
		h.reference.visible = false
	default:
		t.Fatalf("unknown registry model command %q", command)
	}
}

func (h *registryModelHarness) rebuild(t *testing.T) {
	t.Helper()
	defs, err := h.store.ListForProject(context.Background(), h.projectID)
	require.NoError(t, err)
	configs := make(map[string]mcp.ServerConfig, len(defs))
	for _, def := range defs {
		configs[def.Name] = mcp.ServerConfig{Command: def.Command, Args: def.Args, Env: def.Env}
	}

	service, err := mcp.AcquireForWorkDir(context.Background(), h.pool, configs, h.workDir, nil)
	require.NoError(t, err)
	h.service = service
	actual := service != nil && service.Stats().Started == 1
	expected := h.reference.registered && h.reference.enabled
	assert.Equal(t, expected, actual, "real registry projection and pool acquire")
	h.reference.visible = expected
}

func (h *registryModelHarness) assertVisible(t *testing.T, want bool, message string) {
	t.Helper()
	assert.Equal(t, want, h.reference.visible, "reference model: "+message)
	actual := h.service != nil && h.service.Stats().Started == 1
	assert.Equal(t, want, actual, "real registry/pool state: "+message)
}

func (h *registryModelHarness) close() {
	if h.service != nil {
		h.service.Stop()
	}
	h.pool.Stop()
	_ = h.db.Close()
}
