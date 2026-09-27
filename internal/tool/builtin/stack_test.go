package builtin

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/sandboxpolicy"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

func TestStackClose_ClosesIdleWebConnections(t *testing.T) {
	idle := make(chan struct{}, 1)
	closed := make(chan struct{}, 1)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ready")
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		switch state {
		case http.StateNew, http.StateActive, http.StateHijacked:
		case http.StateIdle:
			select {
			case idle <- struct{}{}:
			default:
			}
		case http.StateClosed:
			select {
			case closed <- struct{}{}:
			default:
			}
		}
	}
	server.Start()
	defer server.Close()
	transport := newRestrictedTransport()
	client := &http.Client{Transport: transport}
	response, err := client.Get(server.URL)
	require.NoError(t, err)
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(t, err)
	require.NoError(t, response.Body.Close())
	select {
	case <-idle:
	case <-time.After(2 * time.Second):
		t.Fatal("web connection did not become idle")
	}
	stack := &Stack{web: []*http.Transport{transport}}
	require.NoError(t, stack.Close())
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("stack close kept the web connection open")
	}
}

func TestBuildStack_Independence(t *testing.T) {
	todoA := todo.New()
	todoB := todo.New()

	stackA, err := BuildStack(context.Background(), StackConfig{
		WorkDir: t.TempDir(),
		Loader:  loader.New(),
		Todo:    todoA,
	})
	require.NoError(t, err)

	t.Cleanup(func() { _ = stackA.Close() })

	stackB, err := BuildStack(context.Background(), StackConfig{
		WorkDir: t.TempDir(),
		Loader:  loader.New(),
		Todo:    todoB,
	})
	require.NoError(t, err)

	t.Cleanup(func() { _ = stackB.Close() })

	assert.Equal(t, stackA.Registry.IDs(), stackB.Registry.IDs(), "registries should have same tool set")
	assert.NotSame(
		t,
		stackA.Registry.Get("read"),
		stackB.Registry.Get("read"),
		"registries should have different tool instances",
	)

	todoA.Replace([]*todo.Item{{ID: "1", Content: "only in A"}})
	assert.Len(t, todoA.List(), 1)
	assert.Empty(t, todoB.List(), "todoB should be empty — isolated from todoA")
}

func TestBuildStack_ToolCount(t *testing.T) {
	stack, err := BuildStack(context.Background(), StackConfig{
		WorkDir: t.TempDir(),
		Loader:  loader.New(),
		Todo:    todo.New(),
	})
	require.NoError(t, err)

	t.Cleanup(func() { _ = stack.Close() })

	tools := stack.Registry.IDs()
	assert.Contains(t, tools, "read")
	assert.Contains(t, tools, "write")
	assert.Contains(t, tools, "bash")
	assert.Contains(t, tools, "grep")
	// Memory and task are registered by the session, not the stack.
	assert.NotContains(t, tools, "task")
	assert.NotContains(t, tools, "memory_save")
	assert.NotContains(t, tools, "memory_delete")
}

func TestBuildStack_BashSandboxConfigurationError(t *testing.T) {
	restore := coagenthome.Override(t.TempDir())
	t.Cleanup(restore)

	workDir := t.TempDir()
	unified := &config.UnifiedConfig{}
	unified.Sandbox.Enabled = true
	// A declared rule path must be absolute; the policy compiler refuses the
	// section before any sandbox process is constructed.
	unified.Sandbox.Rules = []sandboxpolicy.Rule{{Allow: "relative/cache", Mode: sandboxpolicy.ModeReadWrite}}

	stack, err := BuildStack(context.Background(), StackConfig{
		WorkDir: workDir,
		Unified: unified,
		Loader:  loader.New(),
		Todo:    todo.New(),
	})
	require.Error(t, err)
	assert.Nil(t, stack)
	assert.Contains(t, err.Error(), "compile sandbox policy")
}

func TestBashSandboxConfig_NilUnified(t *testing.T) {
	cfg, err := bashSandboxConfig(StackConfig{WorkDir: "/tmp/project"}, "/tmp/project")

	require.NoError(t, err)
	assert.False(t, cfg.Enabled)
	assert.Equal(t, "/tmp/project", cfg.WorkDir)
	assert.Empty(t, cfg.Policy.Digest)
}

func TestBashSandboxConfig_Configured(t *testing.T) {
	restore := coagenthome.Override(t.TempDir())
	t.Cleanup(restore)

	workDir := t.TempDir()
	canonicalRoot, err := filepath.EvalSymlinks(workDir)
	require.NoError(t, err)

	home, err := coagenthome.UserHome()
	require.NoError(t, err)

	unified := &config.UnifiedConfig{}
	unified.Sandbox.Enabled = true
	unified.Sandbox.Rules = []sandboxpolicy.Rule{
		{Allow: filepath.Join(home, "tool-state"), Mode: sandboxpolicy.ModeReadWrite},
		{Allow: "/tmp/build-cache", Mode: sandboxpolicy.ModeReadWrite},
	}

	cfg, err := bashSandboxConfig(StackConfig{WorkDir: workDir, Unified: unified}, canonicalRoot)

	require.NoError(t, err)
	assert.True(t, cfg.Enabled)
	assert.Equal(t, workDir, cfg.WorkDir)
	assert.NotEmpty(t, cfg.Policy.Digest)
	assert.True(t, cfg.Policy.AllowsRead("/etc/passwd"), "an ordinary session reads the host filesystem")

	entry, ok := entryWithPath(cfg.Policy.Entries, filepath.Join(home, "tool-state"))
	require.True(t, ok)
	assert.Equal(t, sandboxpolicy.ModeReadWrite, entry.Mode)

	entry, ok = entryWithPath(cfg.Policy.Entries, "/tmp/build-cache")
	require.True(t, ok)
	assert.Equal(t, sandboxpolicy.ModeReadWrite, entry.Mode)
}

func entryWithPath(entries []sandboxpolicy.Entry, path string) (sandboxpolicy.Entry, bool) {
	for _, entry := range entries {
		if entry.Path == path {
			return entry, true
		}
	}

	return sandboxpolicy.Entry{}, false
}

func TestBashSandboxConfig_WorktreeTrustsMainGitDir(t *testing.T) {
	restore := coagenthome.Override(t.TempDir())
	t.Cleanup(restore)

	repoRoot := t.TempDir()
	gitDir := filepath.Join(repoRoot, ".git")
	require.NoError(t, os.Mkdir(gitDir, 0o755))

	// The work tree name contains a dot: the trusted path must not be
	// derived from the work tree basename.
	workDir := filepath.Join(t.TempDir(), "worktrees", "repo-a1b2", "fix-release.v2")
	require.NoError(t, os.MkdirAll(workDir, 0o755))
	canonicalRoot, err := filepath.EvalSymlinks(workDir)
	require.NoError(t, err)

	unified := &config.UnifiedConfig{}
	unified.Sandbox.Enabled = true

	cfg, err := bashSandboxConfig(StackConfig{
		WorkDir: workDir, RepoRoot: repoRoot, Unified: unified,
	}, canonicalRoot)

	require.NoError(t, err)
	assert.True(t, cfg.Enabled)

	entry, ok := entryWithPath(cfg.Policy.Entries, gitDir)
	require.True(t, ok, "a worktree session must receive a read-write entry for the main git dir")
	assert.Equal(t, sandboxpolicy.ModeReadWrite, entry.Mode)

	plain, err := bashSandboxConfig(StackConfig{WorkDir: workDir, Unified: unified}, canonicalRoot)

	require.NoError(t, err)
	_, ok = entryWithPath(plain.Policy.Entries, gitDir)
	assert.False(t, ok, "a plain session must not receive an entry for an unrelated repository's git dir")
}

func TestRegisterCoreTools_SharesFileMutator(t *testing.T) {
	registry := tool.NewRegistry()
	mutator := &recordingFileMutator{}

	registerCoreTools(
		registry,
		t.TempDir(),
		nil,
		loader.New(),
		todo.New(),
		nil,
		nil,
		&bashRunnerStub{},
		mutator,
		nil,
		nil,
		"project-1",
		1,
		1,
		nil,
	)

	assert.Equal(t, mutator, registry.Get("write").(*writeTool).mutator)
	assert.Equal(t, mutator, registry.Get("edit").(*editTool).mutator)
	assert.Equal(t, mutator, registry.Get("apply_patch").(*applyPatchTool).mutator)
}

func TestStackCloseToleratesUnsetOwners(t *testing.T) {
	assert.NoError(t, (&Stack{}).Close())
}

// A broken MCP server leaves the builtins available and reports its failure.
func TestBuildStackKeepsBuiltinsWhenMCPStartFails(t *testing.T) {
	tests := []struct {
		name     string
		servers  map[string]mcp.ServerConfig
		wantWarn bool
	}{
		{name: "server fails", servers: map[string]mcp.ServerConfig{
			"demo": {Command: "coagent-absent-mcp-binary"},
		}, wantWarn: true},
		{name: "no servers", wantWarn: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			ctx := logger.ToContext(context.Background(), zap.New(core))

			stack, err := BuildStack(ctx, StackConfig{
				WorkDir: t.TempDir(),
				Servers: tt.servers,
				Loader:  loader.New(),
				Todo:    todo.New(),
			})
			require.NoError(t, err)

			t.Cleanup(func() { _ = stack.Close() })

			assert.Contains(t, stack.Registry.IDs(), "read", "builtins survive an MCP failure")
			assert.Equal(t, tt.wantWarn, logs.FilterMessage("server_failed").Len() == 1)
		})
	}
}
