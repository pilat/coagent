package lsp

import (
	"context"
	"fmt"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManager_GetClient_CachesPerKey(t *testing.T) {
	spawnCount := 0
	m := &manager{
		servers: []serverConfig{
			{
				ID:         "test-server",
				Extensions: []string{".go"},
				RootFinder: func(workDir, file string) (string, error) {
					return "/project/root", nil
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
		clients: make(map[string]*client),
	}

	ctx := context.Background()

	// First call: spawn is called, returns error.
	_, err1 := m.getClient(ctx, "/project", "/project/root/main.go")
	require.Error(t, err1)
	assert.Equal(t, 1, spawnCount)

	// Manually inject a client for the same key to test cache hit.
	testClient := newTestClient(t, func(req Request) (any, error) {
		return nil, nil
	})
	m.mu.Lock()
	m.clients["test-server:/project/root"] = testClient
	m.mu.Unlock()

	// Second call: should find the cached client, NOT call Spawn again.
	cl, err2 := m.getClient(ctx, "/project", "/project/root/main.go")
	require.NoError(t, err2)
	assert.Equal(t, testClient, cl)
	assert.Equal(t, 1, spawnCount, "Spawn should not be called again")
}

func TestManager_GetClient_DifferentRoots(t *testing.T) {
	m := &manager{
		servers: []serverConfig{
			{
				ID:         "test-server",
				Extensions: []string{".go"},
				RootFinder: func(workDir, file string) (string, error) {
					// Return different roots based on file path.
					if file == "/project/a/main.go" {
						return "/project/a", nil
					}
					return "/project/b", nil
				},
				Spawn: func(ctx context.Context, root string) (*exec.Cmd, error) {
					return nil, fmt.Errorf("no real server")
				},
			},
		},
		clients: make(map[string]*client),
	}

	// Inject two different clients for two different roots.
	clientA := newTestClient(t, func(req Request) (any, error) { return "A", nil })
	clientB := newTestClient(t, func(req Request) (any, error) { return "B", nil })

	m.mu.Lock()
	m.clients["test-server:/project/a"] = clientA
	m.clients["test-server:/project/b"] = clientB
	m.mu.Unlock()

	ctx := context.Background()

	gotA, err := m.getClient(ctx, "/project", "/project/a/main.go")
	require.NoError(t, err)
	assert.Equal(t, clientA, gotA)

	gotB, err := m.getClient(ctx, "/project", "/project/b/main.go")
	require.NoError(t, err)
	assert.Equal(t, clientB, gotB)

	assert.NotEqual(t, gotA, gotB, "different roots should yield different clients")
}

func TestManager_GetClient_NoServerForExtension(t *testing.T) {
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
		clients: make(map[string]*client),
	}

	ctx := context.Background()
	_, err := m.getClient(ctx, "/project", "/project/main.rs")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no LSP server for extension .rs")
}

func TestManager_Close(t *testing.T) {
	clientA := newTestClient(t, func(req Request) (any, error) { return nil, nil })
	clientB := newTestClient(t, func(req Request) (any, error) { return nil, nil })

	m := &manager{
		clients: map[string]*client{
			"server-a:/root/a": clientA,
			"server-b:/root/b": clientB,
		},
	}

	m.Close()

	m.mu.RLock()
	assert.Empty(t, m.clients, "all clients should be removed after Close")
	m.mu.RUnlock()
}

func TestManager_WorkspaceSymbol(t *testing.T) {
	t.Run("finds client by workDir prefix", func(t *testing.T) {
		testClient := newTestClient(t, func(req Request) (any, error) {
			assert.Equal(t, "workspace/symbol", req.Method)
			return []SymbolInformation{
				{Name: "Foo", Kind: 12, Location: Location{URI: "file:///project/foo.go"}},
			}, nil
		})

		m := &manager{
			clients: map[string]*client{
				"gopls:/project": testClient,
			},
		}

		ctx := context.Background()
		symbols, err := m.WorkspaceSymbol(ctx, "/project", "Foo")

		require.NoError(t, err)
		require.Len(t, symbols, 1)
		assert.Equal(t, "Foo", symbols[0].Name)
	})

	t.Run("falls back to any client", func(t *testing.T) {
		testClient := newTestClient(t, func(req Request) (any, error) {
			return []SymbolInformation{
				{Name: "Bar", Kind: 5},
			}, nil
		})

		m := &manager{
			clients: map[string]*client{
				"gopls:/other/project": testClient,
			},
		}

		ctx := context.Background()
		symbols, err := m.WorkspaceSymbol(ctx, "/unrelated", "Bar")

		require.NoError(t, err)
		require.Len(t, symbols, 1)
		assert.Equal(t, "Bar", symbols[0].Name)
	})

	t.Run("no clients available", func(t *testing.T) {
		m := &manager{
			clients: make(map[string]*client),
		}

		ctx := context.Background()
		_, err := m.WorkspaceSymbol(ctx, "/project", "anything")

		require.Error(t, err)
		assert.Contains(t, err.Error(), "no LSP client available")
	})
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
		clients: map[string]*client{
			"gopls:/project": clientA,
		},
	}

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
		clients: map[string]*client{"gopls:/": cl},
	}

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
