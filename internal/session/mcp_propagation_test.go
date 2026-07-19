package session

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/mcpstore"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool/builtin"
)

// fakeMCPServer returns the stdio server under testdata, restoring the executable
// bit in case a checkout dropped it.
func fakeMCPServer(t *testing.T) string {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("the fake MCP server is a POSIX shell script")
	}

	path, err := filepath.Abs(filepath.Join("testdata", "fakemcp.sh"))
	require.NoError(t, err)

	// The MCP client execs this directly, so it has to stay executable even if a
	// checkout dropped the bit.
	require.NoError(t, os.Chmod(path, 0o700)) //nolint:gosec // a test fixture the test itself runs

	return path
}

// buildStackFor rebuilds a session's tool stack the way runSessionIteration does:
// read the registry now, resolve, hand the definitions to BuildStack.
func buildStackFor(t *testing.T, store mcpstore.Store, projectID int64) []string {
	t.Helper()

	ctx := context.Background()

	stack, err := builtin.BuildStack(ctx, builtin.StackConfig{
		WorkDir: t.TempDir(),
		Servers: resolveMCPServers(ctx, store, nil, projectID),
		Loader:  loader.New(),
		Todo:    todo.New(),
	})
	require.NoError(t, err)

	t.Cleanup(func() { _ = stack.Close() })

	var mcpTools []string

	for _, id := range stack.Registry.IDs() {
		if strings.HasPrefix(id, "mcp__") {
			mcpTools = append(mcpTools, id)
		}
	}

	return mcpTools
}

// The whole propagation contract in one test: a registry change is invisible to
// the current stack and present in the next one.
func TestMCPRegistryChangesReachTheNextStackBuild(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newMCPTestStore(t)

	assert.Empty(t, buildStackFor(t, store, projectID), "nothing registered yet")

	require.NoError(t, store.Add(ctx, &projectID, mcpstore.ServerDef{
		Name:    "fake",
		Command: fakeMCPServer(t),
		Enabled: true,
	}))

	assert.Equal(t, []string{"mcp__fake__ping"}, buildStackFor(t, store, projectID),
		"the next build picks up the added server")

	require.NoError(t, store.SetEnabled(ctx, &projectID, "fake", false))
	assert.Empty(t, buildStackFor(t, store, projectID), "disabling removes it from the next build")

	require.NoError(t, store.SetEnabled(ctx, &projectID, "fake", true))
	require.NoError(t, store.Remove(ctx, &projectID, "fake"))
	assert.Empty(t, buildStackFor(t, store, projectID), "removal removes it from the next build")
}

// A global server reaches every project, and a project row of the same name wins.
func TestMCPGlobalServersReachTheStack(t *testing.T) {
	ctx := context.Background()
	store, _, projectID := newMCPTestStore(t)

	require.NoError(t, store.Add(ctx, nil, mcpstore.ServerDef{
		Name:    "shared",
		Command: fakeMCPServer(t),
		Enabled: true,
	}))

	assert.Equal(t, []string{"mcp__shared__ping"}, buildStackFor(t, store, projectID))
}
