package builtin

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/bashsandbox"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/mcp"
	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/projectpath"
	"github.com/pilat/coagent/internal/sandboxpolicy"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

type testNetworkOwner struct{}

type testNetworkLease struct{}

func (testNetworkOwner) Acquire(context.Context, int64, sandboxpolicy.Policy) (NetworkLease, error) {
	return testNetworkLease{}, nil
}

func (testNetworkLease) Link() *bashsandbox.NetworkLink     { return nil }
func (testNetworkLease) BindRunner(procexec.Runner, string) {}
func (testNetworkLease) Release()                           {}
func (testNetworkLease) DialContext(context.Context, string, string) (net.Conn, error) {
	return nil, errors.New("network unavailable in filesystem fixture")
}

func TestBuildStack_EnabledSandboxRequiresNetworkOwner(t *testing.T) {
	restore := coagenthome.Override(t.TempDir())
	t.Cleanup(restore)
	unified := &config.UnifiedConfig{}
	unified.Sandbox.Enabled = true
	stack, err := BuildStack(t.Context(), StackConfig{
		WorkDir: t.TempDir(), SessionID: 1, Unified: unified,
		Loader: loader.New(), Todo: todo.New(),
	})
	require.Nil(t, stack)
	require.ErrorContains(t, err, "network owner is unavailable")
}

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
	// A declared writable path must be absolute; the policy compiler refuses the
	// section before any sandbox process is constructed.
	unified.Sandbox.WritablePaths = []string{"relative/cache"}

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
	cfg, err := bashSandboxConfig(t.Context(), StackConfig{WorkDir: "/tmp/project"}, "/tmp/project")

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

	unified := &config.UnifiedConfig{}
	unified.Sandbox.Enabled = true
	unified.Sandbox.WritablePaths = []string{"~/.cache", "/tmp/build-cache"}

	cfg, err := bashSandboxConfig(t.Context(), StackConfig{WorkDir: workDir, Unified: unified}, canonicalRoot)

	require.NoError(t, err)
	assert.True(t, cfg.Enabled)
	assert.Equal(t, workDir, cfg.WorkDir)
	assert.NotEmpty(t, cfg.Policy.Digest)

	home, err := coagenthome.UserHome()
	require.NoError(t, err)

	legacy := map[string]sandboxpolicy.Mode{}
	for _, grant := range cfg.Policy.Grants {
		if grant.Profile == "legacy" {
			legacy[grant.Target] = grant.Mode
		}
	}

	assert.Equal(t, sandboxpolicy.ModeReadWrite, legacy[filepath.Join(home, ".cache")])
	assert.Equal(t, sandboxpolicy.ModeReadWrite, legacy["/tmp/build-cache"])
	assert.Len(t, legacy, 2)
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

	for _, shieldsUp := range []bool{false, true} {
		cfg, err := bashSandboxConfig(t.Context(), StackConfig{
			WorkDir: workDir, RepoRoot: repoRoot, Unified: unified, ShieldsUp: shieldsUp,
		}, canonicalRoot)

		require.NoError(t, err)
		assert.True(t, cfg.Enabled)

		var trusted bool
		for _, grant := range cfg.Policy.Grants {
			if grant.Target != gitDir {
				continue
			}

			trusted = true
			assert.Equal(t, sandboxpolicy.ModeReadWrite, grant.Mode)
		}

		assert.True(t, trusted,
			"shieldsUp=%t: a worktree session must receive a read-write grant for the main git dir", shieldsUp)
	}

	plain, err := bashSandboxConfig(t.Context(), StackConfig{WorkDir: workDir, Unified: unified}, canonicalRoot)

	require.NoError(t, err)
	for _, grant := range plain.Policy.Grants {
		assert.NotEqual(t, gitDir, grant.Target,
			"a plain session must not receive a grant for an unrelated repository's git dir")
	}
}

func TestProjectEscalation_WorktreeInheritsOnlyItsSourceProject(t *testing.T) {
	repoRoot := t.TempDir()
	otherRoot := t.TempDir()
	worktreesRoot := t.TempDir()
	worktree := filepath.Join(worktreesRoot, "repo", "feature")
	otherWorktree := filepath.Join(worktreesRoot, "other", "feature")
	projects := map[string]sandboxpolicy.ProjectOverride{
		repoRoot:      {Escalated: []string{"gh", "ssh"}},
		worktree:      {Escalated: []string{"docker", "gh"}},
		otherRoot:     {Escalated: []string{"mise"}},
		otherWorktree: {Escalated: []string{"node"}},
	}

	got, err := projectEscalation(projects, worktree, repoRoot)
	require.NoError(t, err)
	assert.Equal(t, []string{"docker", "gh", "ssh"}, got)

	got, err = projectEscalation(projects, otherWorktree, otherRoot)
	require.NoError(t, err)
	assert.Equal(t, []string{"mise", "node"}, got)

	got, err = projectEscalation(projects, filepath.Join(repoRoot, "nested"), "")
	require.NoError(t, err)
	assert.Empty(t, got)

	got, err = projectEscalation(projects, worktree, "")
	require.NoError(t, err)
	assert.Equal(t, []string{"docker", "gh"}, got)
}

func TestBashSandboxConfig_WorktreeEscalationUnionAndShields(t *testing.T) {
	restore := coagenthome.Override(t.TempDir())
	t.Cleanup(restore)

	repoRoot := t.TempDir()
	worktreesRoot := t.TempDir()
	worktree := projectpath.WorktreePath(worktreesRoot, repoRoot, "feature")
	createLinkedWorktreeFixture(t, repoRoot, worktree)

	unified := &config.UnifiedConfig{WorktreesRoot: worktreesRoot}
	unified.Sandbox.Enabled = true
	unified.Sandbox.Escalated = []string{"global_test"}
	unified.Sandbox.Projects = map[string]sandboxpolicy.ProjectOverride{
		repoRoot: {Escalated: []string{"source_test"}},
		worktree: {Escalated: []string{"worktree_test"}},
	}
	unified.Sandbox.Profiles = make(map[string]sandboxpolicy.Profile)
	for _, name := range []string{"global_test", "source_test", "worktree_test"} {
		path := filepath.Join(t.TempDir(), name)
		require.NoError(t, os.WriteFile(path, []byte(name), 0o600))
		unified.Sandbox.Profiles[name] = sandboxpolicy.Profile{Mounts: []sandboxpolicy.Mount{{
			Path: path, Mode: sandboxpolicy.ModeReadOnly, Type: sandboxpolicy.LevelEscalated,
		}}}
	}

	for _, scenario := range []struct {
		shieldsUp bool
		created   bool
		want      map[string]bool
	}{
		{created: true, want: map[string]bool{
			"global_test": true, "source_test": true, "worktree_test": true,
		}},
		{created: false, want: map[string]bool{
			"global_test": true, "worktree_test": true,
		}},
		{shieldsUp: true, created: true, want: map[string]bool{}},
	} {
		cfg, err := bashSandboxConfig(t.Context(), StackConfig{
			WorkDir: worktree, RepoRoot: repoRoot, CreatedWorktree: scenario.created,
			Unified: unified, ShieldsUp: scenario.shieldsUp,
		}, worktree)
		require.NoError(t, err)
		profiles := make(map[string]bool)
		for _, grant := range cfg.Policy.Grants {
			if grant.Level == sandboxpolicy.LevelEscalated {
				profiles[grant.Profile] = true
			}
		}
		assert.Equal(t, scenario.want, profiles)
	}
}

func TestVerifiedWorktreeSource_RejectsHistoricalSpoof(t *testing.T) {
	repoRoot := t.TempDir()
	otherRoot := t.TempDir()
	worktreesRoot := t.TempDir()
	worktree := projectpath.WorktreePath(worktreesRoot, repoRoot, "feature")
	createLinkedWorktreeFixture(t, otherRoot, worktree)

	assert.Empty(t, verifiedWorktreeSource(t.Context(), worktree, repoRoot, worktreesRoot, true),
		"matching the /gwt namespace without matching Git ancestry must not inherit grants")
	assert.Empty(t, verifiedWorktreeSource(t.Context(), worktree, otherRoot, worktreesRoot, true),
		"matching Git ancestry outside its namespace must not inherit grants")
	assert.Empty(t, verifiedWorktreeSource(t.Context(), worktree, otherRoot, worktreesRoot, false),
		"historical attributes without controller provenance cannot inherit")
}

func createLinkedWorktreeFixture(t *testing.T, repoRoot, worktree string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-q", repoRoot},
		{
			"-C", repoRoot, "-c", "user.name=Test", "-c", "user.email=test@example.invalid",
			"-c", "commit.gpgsign=false", "commit", "-q", "--allow-empty", "-m", "initial",
		},
		{"-C", repoRoot, "worktree", "add", "--detach", worktree},
	} {
		output, err := exec.CommandContext(t.Context(), "git", args...).CombinedOutput()
		require.NoError(t, err, "%s", output)
	}
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
