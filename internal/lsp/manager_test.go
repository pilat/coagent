package lsp

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManager_GetClient_CachesPerKey(t *testing.T) {
	workDir := t.TempDir()
	root := filepath.Join(workDir, "root")
	require.NoError(t, os.MkdirAll(root, 0o755))
	spawnCount := 0
	m := &manager{
		servers: []serverConfig{
			{
				ID:         "test-server",
				Extensions: []string{".go"},
				RootFinder: func(workDir, file string) (string, error) {
					return root, nil
				},
				Spawn: func(ctx context.Context, root string) (*exec.Cmd, error) {
					spawnCount++
					// Return an error to avoid actually starting a process.
					// We only care that the cache key logic works — and the second
					// call hits the cache before reaching Spawn.
					return nil, fmt.Errorf("no real server")
				},
			},
		},
		clients: make(map[clientKey]*client),
	}
	ctx := context.Background()

	// First call: spawn is called, returns error.
	_, err1 := m.getClient(ctx, workDir, filepath.Join(root, "main.go"))
	require.Error(t, err1)
	assert.Equal(t, 1, spawnCount)

	// Manually inject a client for the same key to test cache hit.
	testClient := newTestClient(t, func(req Request) (any, error) {
		return nil, nil
	})
	m.mu.Lock()
	m.clients[clientKey{serverID: "test-server", root: root}] = testClient
	m.mu.Unlock()

	// Second call: should find the cached client, NOT call Spawn again.
	cl, err2 := m.getClient(ctx, workDir, filepath.Join(root, "main.go"))
	require.NoError(t, err2)
	assert.Equal(t, testClient, cl)
	assert.Equal(t, 1, spawnCount, "Spawn should not be called again")
}

func TestManager_GetClient_DifferentRoots(t *testing.T) {
	workDir := t.TempDir()
	rootA := filepath.Join(workDir, "a")
	rootB := filepath.Join(workDir, "b")
	require.NoError(t, os.MkdirAll(rootA, 0o755))
	require.NoError(t, os.MkdirAll(rootB, 0o755))
	m := &manager{
		servers: []serverConfig{
			{
				ID:         "test-server",
				Extensions: []string{".go"},
				RootFinder: func(workDir, file string) (string, error) {
					// Return different roots based on file path.
					if file == filepath.Join(rootA, "main.go") {
						return rootA, nil
					}
					return rootB, nil
				},
				Spawn: func(ctx context.Context, root string) (*exec.Cmd, error) {
					return nil, fmt.Errorf("no real server")
				},
			},
		},
		clients: make(map[clientKey]*client),
	}

	// Inject two different clients for two different roots.
	clientA := newTestClient(t, func(req Request) (any, error) { return "A", nil })
	clientB := newTestClient(t, func(req Request) (any, error) { return "B", nil })

	m.mu.Lock()
	m.clients[clientKey{serverID: "test-server", root: rootA}] = clientA
	m.clients[clientKey{serverID: "test-server", root: rootB}] = clientB
	m.mu.Unlock()

	ctx := context.Background()

	gotA, err := m.getClient(ctx, workDir, filepath.Join(rootA, "main.go"))
	require.NoError(t, err)
	assert.Equal(t, clientA, gotA)

	gotB, err := m.getClient(ctx, workDir, filepath.Join(rootB, "main.go"))
	require.NoError(t, err)
	assert.Equal(t, clientB, gotB)

	assert.NotEqual(t, gotA, gotB, "different roots should yield different clients")
}

func TestManager_GetClient_NoServerForExtension(t *testing.T) {
	workDir := t.TempDir()
	m := &manager{
		servers: []serverConfig{
			{
				ID:         "go-only",
				Extensions: []string{".go"},
				RootFinder: func(workDir, file string) (string, error) {
					return workDir, nil
				},
			},
		},
		clients: make(map[clientKey]*client),
	}

	ctx := context.Background()
	_, err := m.getClient(ctx, workDir, filepath.Join(workDir, "main.rs"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no LSP server for file")
}

func TestManager_Close(t *testing.T) {
	clientA := newTestClient(t, func(req Request) (any, error) { return nil, nil })
	clientB := newTestClient(t, func(req Request) (any, error) { return nil, nil })

	m := &manager{
		clients: map[clientKey]*client{
			{serverID: "server-a", root: "/root/a"}: clientA,
			{serverID: "server-b", root: "/root/b"}: clientB,
		},
	}

	m.Close()

	m.mu.RLock()
	assert.Empty(t, m.clients, "all clients should be removed after Close")
	m.mu.RUnlock()
}

func TestManager_WorkspaceSymbol(t *testing.T) {
	workDir := t.TempDir()
	file := filepath.Join(workDir, "main.go")
	require.NoError(t, os.WriteFile(file, []byte("package main\n"), 0o644))
	testClient := newTestClient(t, func(req Request) (any, error) {
		assert.Equal(t, "workspace/symbol", req.Method)
		return []SymbolInformation{{Name: "Foo", Kind: 12, Location: SymbolLocation{URI: fileURI(file)}}}, nil
	})
	m := &manager{
		servers: []serverConfig{
			{
				ID:         "gopls",
				Extensions: []string{".go"},
				RootFinder: func(_, _ string) (string, error) { return workDir, nil },
			},
		},
		clients: map[clientKey]*client{{serverID: "gopls", root: workDir}: testClient},
	}
	symbols, err := m.WorkspaceSymbol(context.Background(), workDir, file, "Foo")
	require.NoError(t, err)
	require.Len(t, symbols, 1)
	assert.Equal(t, "Foo", symbols[0].Name)
}

func TestManager_GetAllDiagnostics(t *testing.T) {
	clientA := newClient()
	clientA.diagnosticsMu.Lock()
	clientA.diagnostics["file:///project/a.go"] = []Diagnostic{
		{Severity: 1, Message: "error a1"},
		{Severity: 2, Message: "warning a1"}, // Should be excluded (severity != 1).
		{Severity: 1, Message: "error a2"},
	}
	clientA.diagnostics["file:///project/b.go"] = []Diagnostic{
		{Severity: 2, Message: "warning b1"}, // No errors — should not appear.
	}
	clientA.diagnosticsMu.Unlock()

	m := &manager{
		clients: map[clientKey]*client{
			{serverID: "gopls", root: "/project"}: clientA,
		},
	}
	clientA.rootPath = "/project"

	ctx := context.Background()
	result := m.GetAllDiagnostics(ctx, "/project", 10, 10)

	// Only a.go has severity=1 errors.
	require.Len(t, result, 1)
	assert.Equal(t, "/project/a.go", result[0].Path)
	assert.Len(t, result[0].Diagnostics, 2)

	for _, d := range result[0].Diagnostics {
		assert.Equal(t, 1, d.Severity)
	}
}

func TestManager_GetAllDiagnostics_RespectsLimits(t *testing.T) {
	cl := newClient()
	cl.diagnosticsMu.Lock()
	cl.diagnostics["file:///a.go"] = []Diagnostic{
		{Severity: 1, Message: "err1"},
		{Severity: 1, Message: "err2"},
		{Severity: 1, Message: "err3"},
	}
	cl.diagnostics["file:///b.go"] = []Diagnostic{
		{Severity: 1, Message: "err4"},
	}
	cl.diagnosticsMu.Unlock()

	m := &manager{
		clients: map[clientKey]*client{{serverID: "gopls", root: "/"}: cl},
	}
	cl.rootPath = "/"

	ctx := context.Background()

	t.Run("maxErrorsPerFile limits errors", func(t *testing.T) {
		result := m.GetAllDiagnostics(ctx, "/", 2, 10)
		for _, fd := range result {
			assert.LessOrEqual(t, len(fd.Diagnostics), 2)
		}
	})

	t.Run("maxFiles limits files", func(t *testing.T) {
		result := m.GetAllDiagnostics(ctx, "/", 10, 1)
		assert.Len(t, result, 1)
	})
}

func TestManagerGetAllDiagnosticsFiltersClientRootsAndExitedClients(t *testing.T) {
	inside := newClient()
	inside.rootPath = "/project/module"
	inside.diagnostics["file:///project/module/main.go"] = []Diagnostic{{Severity: 1, Message: "inside"}}

	exited := newClient()
	exited.rootPath = "/project"
	exited.diagnostics["file:///project/exited.go"] = []Diagnostic{{Severity: 1, Message: "exited"}}
	exited.exited.Store(true)

	outside := newClient()
	outside.rootPath = "/other"
	outside.diagnostics["file:///project/leak.go"] = []Diagnostic{{Severity: 1, Message: "leak"}}

	m := &manager{clients: map[clientKey]*client{
		{serverID: "inside", root: inside.rootPath}:   inside,
		{serverID: "exited", root: exited.rootPath}:   exited,
		{serverID: "outside", root: outside.rootPath}: outside,
	}}

	result := m.GetAllDiagnostics(context.Background(), "/project", 10, 10)
	require.Len(t, result, 1)
	assert.Equal(t, "/project/module/main.go", result[0].Path)
}
