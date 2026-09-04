package builtin

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

func TestBuildStack_Independence(t *testing.T) {
	todoA := todo.New()
	todoB := todo.New()

	stackA, err := BuildStack(context.Background(), StackConfig{
		WorkDir: "/tmp/project-a",
		Loader:  loader.New(),
		Todo:    todoA,
	})
	require.NoError(t, err)

	t.Cleanup(func() { _ = stackA.Close() })

	stackB, err := BuildStack(context.Background(), StackConfig{
		WorkDir: "/tmp/project-b",
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
		WorkDir: "/tmp/test",
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
	workDir := t.TempDir()
	unified := &config.UnifiedConfig{}
	unified.Tools.Bash.Sandbox.Enabled = true
	unified.Tools.Bash.Sandbox.WritablePaths = []string{filepath.Join(workDir, "missing")}

	stack, err := BuildStack(context.Background(), StackConfig{
		WorkDir: workDir,
		Unified: unified,
		Loader:  loader.New(),
		Todo:    todo.New(),
	})
	require.Error(t, err)
	assert.Nil(t, stack)
	assert.Contains(t, err.Error(), "create bash sandbox")
}

func TestBashSandboxConfig_NilUnified(t *testing.T) {
	cfg := bashSandboxConfig(StackConfig{WorkDir: "/tmp/project"})

	assert.False(t, cfg.Enabled)
	assert.Equal(t, "/tmp/project", cfg.WorkDir)
	assert.Empty(t, cfg.WritablePaths)
}

func TestBashSandboxConfig_Configured(t *testing.T) {
	unified := &config.UnifiedConfig{}
	unified.Tools.Bash.Sandbox.Enabled = true
	unified.Tools.Bash.Sandbox.WritablePaths = []string{"~/.cache", "/tmp/build-cache"}

	cfg := bashSandboxConfig(StackConfig{WorkDir: "/tmp/project", Unified: unified})

	assert.True(t, cfg.Enabled)
	assert.Equal(t, "/tmp/project", cfg.WorkDir)
	assert.Equal(t, []string{"~/.cache", "/tmp/build-cache"}, cfg.WritablePaths)
}

func TestRegisterCoreTools_SharesFileMutator(t *testing.T) {
	registry := tool.NewRegistry()
	mutator := &recordingFileMutator{}

	registerCoreTools(
		registry,
		t.TempDir(),
		loader.New(),
		todo.New(),
		nil,
		nil,
		&bashRunnerStub{},
		mutator,
	)

	assert.Equal(t, mutator, registry.Get("write").(*writeTool).mutator)
	assert.Equal(t, mutator, registry.Get("edit").(*editTool).mutator)
	assert.Equal(t, mutator, registry.Get("apply_patch").(*applyPatchTool).mutator)
}

func TestStackCloseToleratesUnsetOwners(t *testing.T) {
	assert.NoError(t, (&Stack{}).Close())
}

type stubMCPPool struct {
	err error
}

var _ mcp.Pool = (*stubMCPPool)(nil)

func (p *stubMCPPool) Acquire(
	_ context.Context,
	_ map[string]mcp.ServerConfig,
) (*mcp.Snapshot, error) {
	if p.err != nil {
		return nil, p.err
	}

	return &mcp.Snapshot{}, nil
}

func (p *stubMCPPool) Release([]string) {}
func (p *stubMCPPool) Stop()            {}
func (p *stubMCPPool) ClientFor(context.Context, string, mcp.ServerConfig) (*mcp.Client, error) {
	return nil, nil
}
func (p *stubMCPPool) Invalidate(string) {}

// A broken MCP server degrades the stack to builtins, but it must not do so silently.
func TestBuildStackLogsMCPAcquireFailure(t *testing.T) {
	tests := []struct {
		name     string
		poolErr  error
		wantWarn bool
	}{
		{name: "pool fails", poolErr: errors.New("server unreachable"), wantWarn: true},
		{name: "pool succeeds", poolErr: nil, wantWarn: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			ctx := logger.ToContext(context.Background(), zap.New(core))

			stack, err := BuildStack(ctx, StackConfig{
				WorkDir: t.TempDir(),
				Pool:    &stubMCPPool{err: tt.poolErr},
				Servers: map[string]mcp.ServerConfig{"demo": {Command: "true"}},
				Loader:  loader.New(),
				Todo:    todo.New(),
			})
			require.NoError(t, err)

			t.Cleanup(func() { _ = stack.Close() })

			assert.Contains(t, stack.Registry.IDs(), "read", "builtins survive an MCP failure")
			assert.Equal(t, tt.wantWarn, logs.FilterMessage("mcp_acquire_failed").Len() == 1)
		})
	}
}
