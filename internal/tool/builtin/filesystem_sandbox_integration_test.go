//go:build linux

package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/sandboxpolicy"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

type nativeToolSandbox struct {
	stack      *Stack
	workDir    string
	configured string
	denied     string
}

func TestFilesystemTools_ProcessOutputOverridesDeny(t *testing.T) {
	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap is not installed")
	}
	t.Setenv("SHELL", "/bin/false")
	t.Cleanup(coagenthome.Override(t.TempDir()))
	home, err := coagenthome.Dir()
	require.NoError(t, err)
	outputRoot, err := coagenthome.ProcessProjectDir(1)
	require.NoError(t, err)
	service, db := newTestProcessServiceAt(t, filepath.Dir(outputRoot))
	_, err = db.Exec(`INSERT INTO sessions (id, project_id, parent_id, root_id, model, agent_type)
		VALUES (2, 1, 1, 1, 'm', 'general')`)
	require.NoError(t, err)
	assert.NoDirExists(t, outputRoot)
	workDir := t.TempDir()
	unified := &config.UnifiedConfig{Sandbox: config.SandboxConfig{
		Enabled: true, Rules: []sandboxpolicy.Rule{{Deny: home}},
		Projects: map[string]sandboxpolicy.ProjectRules{workDir: {
			Rules: []sandboxpolicy.Rule{{Allow: outputRoot, Mode: sandboxpolicy.ModeReadWrite}, {Deny: outputRoot}},
		}},
	}}
	stacks := make([]*Stack, 0, 2)
	for _, sessionID := range []int64{1, 2} {
		stack, err := BuildStack(t.Context(), StackConfig{
			ProjectID: 1, SessionID: sessionID, RootSessionID: 1, WorkDir: workDir,
			Unified: unified, ProcessService: service, Loader: loader.New(), Todo: todo.New(),
		})
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, stack.Close()) })
		stacks = append(stacks, stack)
	}
	assert.DirExists(t, outputRoot)

	for i, stack := range stacks {
		result, err := stack.Registry.Get("bash").Execute(tool.WithCallID(t.Context(), fmt.Sprintf("output-%d", i)),
			marshalToolParams(t, bashParams{Command: "printf 'process-output\n'", Background: true}))
		require.NoError(t, err)
		processID, ok := result.Metadata[metaKeyProcessID].(string)
		require.True(t, ok)
		var record backgroundprocess.Process
		require.Eventually(t, func() bool {
			var readErr error
			record, readErr = service.Store().GetProcess(t.Context(), processID)
			return readErr == nil && record.State.Terminal()
		}, 5*time.Second, 10*time.Millisecond)
		require.Equal(t, backgroundprocess.StateCompleted, record.State)
		for _, reader := range stacks {
			assertProcessOutputReadable(t, reader, record.OutputPath, workDir)
		}
	}

	for _, path := range []string{filepath.Join(home, "secrets"), filepath.Join(filepath.Dir(outputRoot), "project-2", "output")} {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
		require.NoError(t, os.WriteFile(path, []byte("hidden"), 0o600))
		_, err := stacks[0].access.Open(path)
		require.Error(t, err)
		cmd, err := stacks[0].runner.BashCommand(t.Context(), "cat "+quoteShell(path), workDir)
		require.NoError(t, err)
		output, err := cmd.CombinedOutput()
		procexec.CloseExtraFiles(cmd)
		require.Error(t, err, string(output))
		assert.NotContains(t, string(output), "hidden")
	}
}

func assertProcessOutputReadable(t *testing.T, stack *Stack, path, workDir string) {
	t.Helper()
	for _, toolID := range []string{"read", "tail"} {
		result, err := stack.Registry.Get(toolID).Execute(t.Context(), marshalToolParams(t, readParams{FilePath: path}))
		require.NoError(t, err, toolID)
		assert.Contains(t, result.Output, "process-output", toolID)
	}
	cmd, err := stack.runner.BashCommand(t.Context(), "cat "+quoteShell(path), workDir)
	require.NoError(t, err)
	output, err := cmd.CombinedOutput()
	procexec.CloseExtraFiles(cmd)
	require.NoError(t, err, string(output))
	assert.Contains(t, string(output), "process-output")

	_, err = stack.access.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o600)
	require.Error(t, err)
	cmd, err = stack.runner.BashCommand(t.Context(), "printf changed > "+quoteShell(path), workDir)
	require.NoError(t, err)
	output, err = cmd.CombinedOutput()
	procexec.CloseExtraFiles(cmd)
	require.Error(t, err, string(output))
	assertTestFileContent(t, path, "process-output\n")
}

func TestFilesystemTools_NativeSandboxMutationPolicy(t *testing.T) {
	fixture := newNativeToolSandbox(t)

	for _, toolID := range []string{"write", "edit", "apply_patch"} {
		t.Run(toolID, func(t *testing.T) {
			impl := fixture.stack.Registry.Get(toolID)
			require.NotNil(t, impl)

			t.Run("allows workspace", func(t *testing.T) {
				path := filepath.Join(fixture.workDir, toolID+"-workspace.txt")
				writeTestFile(t, path, "before")
				require.NoError(t, os.Chmod(path, 0o600))

				require.NoError(t, executeToolMutation(t, impl, path))
				assertTestFileContent(t, path, "after")
				info, err := os.Stat(path)
				require.NoError(t, err)
				assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
			})

			t.Run("allows configured root", func(t *testing.T) {
				path := filepath.Join(fixture.configured, toolID+"-configured.txt")
				writeTestFile(t, path, "before")

				require.NoError(t, executeToolMutation(t, impl, path))
				assertTestFileContent(t, path, "after")
			})

			t.Run("denies existing outside file", func(t *testing.T) {
				path := filepath.Join(fixture.denied, toolID+"-denied.txt")
				writeTestFile(t, path, "before")

				require.Error(t, executeToolMutation(t, impl, path))
				assertTestFileContent(t, path, "before")
			})

			if toolID != "edit" {
				t.Run("denies creation in existing outside directory", func(t *testing.T) {
					path := filepath.Join(fixture.denied, toolID+"-denied-create.txt")

					require.Error(t, executeToolCreation(t, impl, path))
					assert.NoFileExists(t, path)
				})
			}

			t.Run("denies final symlink escape", func(t *testing.T) {
				target := filepath.Join(fixture.denied, toolID+"-final-target.txt")
				link := filepath.Join(fixture.workDir, toolID+"-final-link.txt")
				writeTestFile(t, target, "before")
				require.NoError(t, os.Symlink(target, link))

				require.Error(t, executeToolMutation(t, impl, link))
				assertTestFileContent(t, target, "before")
			})

			t.Run("denies intermediate symlink escape", func(t *testing.T) {
				targetDir := filepath.Join(fixture.denied, toolID+"-intermediate-target")
				linkDir := filepath.Join(fixture.workDir, toolID+"-intermediate-link")
				target := filepath.Join(targetDir, "file.txt")
				link := filepath.Join(linkDir, "file.txt")
				require.NoError(t, os.Mkdir(targetDir, 0o755))
				writeTestFile(t, target, "before")
				require.NoError(t, os.Symlink(targetDir, linkDir))

				require.Error(t, executeToolMutation(t, impl, link))
				assertTestFileContent(t, target, "before")
			})
		})
	}
}

func TestFilesystemTools_NativeSandboxParentCreation(t *testing.T) {
	fixture := newNativeToolSandbox(t)

	for _, toolID := range []string{"write", "apply_patch"} {
		t.Run(toolID, func(t *testing.T) {
			impl := fixture.stack.Registry.Get(toolID)
			require.NotNil(t, impl)

			for name, root := range map[string]string{
				"workspace":       fixture.workDir,
				"configured root": fixture.configured,
			} {
				t.Run("allows parents in "+name, func(t *testing.T) {
					allowed := filepath.Join(root, toolID+"-nested", "file.txt")
					require.NoError(t, executeToolCreation(t, impl, allowed))
					assertTestFileContent(t, allowed, "created")
				})
			}

			t.Run("denies outside parents", func(t *testing.T) {
				deniedParent := filepath.Join(fixture.denied, toolID+"-nested")
				denied := filepath.Join(deniedParent, "file.txt")
				require.Error(t, executeToolCreation(t, impl, denied))
				assert.NoDirExists(t, deniedParent)
				assert.NoFileExists(t, denied)
			})
		})
	}
}

// Every read surface must agree on the ordinary contract: a path outside the
// compiled grants is readable and still not writable. Reads are broad because
// enumerating every tool's configuration never converges; the write boundary is
// the one a mistaken model actually crosses.
func TestFilesystemTools_OrdinaryStackReadsUngrantedPathsButNeverWritesThem(t *testing.T) {
	fixture := newNativeToolSandbox(t)
	path := filepath.Join(fixture.denied, "readable.txt")
	writeTestFile(t, path, "host data")

	tests := []struct {
		toolID string
		params any
	}{
		{
			toolID: "bash",
			params: bashParams{Command: "cat " + quoteShell(path), WorkDir: fixture.workDir},
		},
		{toolID: "read", params: readParams{FilePath: path}},
		{toolID: "ls", params: LsParams{Path: fixture.denied}},
		{toolID: "glob", params: globParams{Pattern: "*.txt", Path: fixture.denied}},
		{toolID: "grep", params: grepParams{Pattern: "host data", Path: path}},
	}

	for _, tt := range tests {
		t.Run(tt.toolID, func(t *testing.T) {
			impl := fixture.stack.Registry.Get(tt.toolID)
			require.NotNil(t, impl)

			params := marshalToolParams(t, tt.params)
			result, err := impl.Execute(context.Background(), params)
			require.NoError(t, err, "an ordinary session reads the host filesystem")
			assert.NotEmpty(t, result.Output)
		})
	}

	// The same path stays unwritable through every mutation surface.
	for _, toolID := range []string{"write", "edit", "apply_patch"} {
		t.Run(toolID+" denies write", func(t *testing.T) {
			impl := fixture.stack.Registry.Get(toolID)
			require.NotNil(t, impl)
			require.Error(t, executeToolMutation(t, impl, path))
			assertTestFileContent(t, path, "host data")
		})
	}
}

func TestFilesystemTools_NativeSandboxBatchCannotBypassWritePolicy(t *testing.T) {
	fixture := newNativeToolSandbox(t)
	path := filepath.Join(fixture.denied, "batch-denied.txt")
	writeParams := marshalToolParams(t, writeParams{FilePath: path, Content: "denied"})
	params := marshalToolParams(t, BatchParams{Calls: []BatchCall{{Tool: "write", Params: writeParams}}})

	result, err := fixture.stack.Registry.Get(tool.IDBatch).Execute(context.Background(), params)

	require.NoError(t, err)
	assert.Equal(t, 1, result.Metadata["errors"])
	assert.Contains(t, result.Output, "Error:")
	assert.NoFileExists(t, path)
}

func TestFilesystemTools_NativeSandboxPatchRemainsSequential(t *testing.T) {
	fixture := newNativeToolSandbox(t)
	allowed := filepath.Join(fixture.workDir, "patch-sequential-allowed.txt")
	denied := filepath.Join(fixture.denied, "patch-sequential-denied.txt")
	writeTestFile(t, allowed, "before")
	writeTestFile(t, denied, "before")
	patch := fmt.Sprintf(
		"--- %[1]s\n+++ %[1]s\n@@ -1,1 +1,1 @@\n-before\n+after\n"+
			"--- %[2]s\n+++ %[2]s\n@@ -1,1 +1,1 @@\n-before\n+after",
		allowed,
		denied,
	)

	result, err := fixture.stack.Registry.Get("apply_patch").Execute(
		context.Background(),
		marshalApplyPatchParams(t, patch),
	)

	require.Error(t, err)
	assert.Nil(t, result)
	assertTestFileContent(t, allowed, "before")
	assertTestFileContent(t, denied, "before")
}

func newNativeToolSandbox(t *testing.T) nativeToolSandbox {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	if _, err := exec.LookPath("bwrap"); err != nil {
		t.Skip("bwrap is not installed")
	}

	base, err := os.MkdirTemp(".", ".coagent-tool-sandbox-test-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(base)) })
	base, err = filepath.Abs(base)
	require.NoError(t, err)

	require.False(t, testPathWithin(base, os.TempDir()), "denied fixture is under implicit temp root")
	if cacheDir, err := os.UserCacheDir(); err == nil {
		require.False(t, testPathWithin(base, cacheDir), "denied fixture is under implicit cache root")
	}

	workDir := filepath.Join(base, "workspace")
	configured := filepath.Join(base, "configured")
	denied := filepath.Join(base, "denied")
	require.NoError(t, os.Mkdir(workDir, 0o755))
	require.NoError(t, os.Mkdir(configured, 0o755))
	require.NoError(t, os.Mkdir(denied, 0o755))

	for name, path := range map[string]string{
		"workspace":       workDir,
		"configured root": configured,
		"denied fixture":  denied,
	} {
		require.False(t, testPathWithin(path, os.TempDir()), name+" is under implicit temp root")
		if cacheDir, err := os.UserCacheDir(); err == nil {
			require.False(t, testPathWithin(path, cacheDir), name+" is under implicit cache root")
		}
	}

	unified := &config.UnifiedConfig{}
	unified.Sandbox.Enabled = true
	unified.Sandbox.Rules = []sandboxpolicy.Rule{{Allow: configured, Mode: sandboxpolicy.ModeReadWrite}}
	stack, err := BuildStack(context.Background(), StackConfig{
		WorkDir: workDir,
		Unified: unified,
		Loader:  loader.New(),
		Todo:    todo.New(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, stack.Close()) })

	return nativeToolSandbox{
		stack:      stack,
		workDir:    workDir,
		configured: configured,
		denied:     denied,
	}
}

func executeToolMutation(t *testing.T, impl tool.Tool, path string) error {
	t.Helper()

	switch impl.ID() {
	case "write":
		_, err := impl.Execute(context.Background(), marshalToolParams(t, writeParams{
			FilePath: path,
			Content:  "after",
		}))
		return err
	case "edit":
		_, err := impl.Execute(context.Background(), marshalToolParams(t, editParams{
			FilePath:  path,
			OldString: "before",
			NewString: "after",
		}))
		return err
	case "apply_patch":
		patch := fmt.Sprintf("--- %s\n+++ %s\n@@ -1,1 +1,1 @@\n-before\n+after", path, path)
		_, err := impl.Execute(context.Background(), marshalApplyPatchParams(t, patch))
		return err
	default:
		t.Fatalf("unsupported mutation tool %q", impl.ID())
		return nil
	}
}

func executeToolCreation(t *testing.T, impl tool.Tool, path string) error {
	t.Helper()

	if impl.ID() == "write" {
		_, err := impl.Execute(context.Background(), marshalToolParams(t, writeParams{
			FilePath: path,
			Content:  "created",
		}))
		return err
	}

	patch := fmt.Sprintf("--- /dev/null\n+++ %s\n@@ -0,0 +1,1 @@\n+created", path)
	_, err := impl.Execute(context.Background(), marshalApplyPatchParams(t, patch))

	return err
}

func marshalToolParams(t *testing.T, params any) json.RawMessage {
	t.Helper()

	data, err := json.Marshal(params)
	require.NoError(t, err)

	return data
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()

	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}

func assertTestFileContent(t *testing.T, path, want string) {
	t.Helper()

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, want, string(content))
}

func testPathWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}

	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
