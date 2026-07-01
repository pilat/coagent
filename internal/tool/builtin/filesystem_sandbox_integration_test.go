//go:build darwin || linux

package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

type nativeToolSandbox struct {
	stack      *Stack
	workDir    string
	configured string
	denied     string
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

func TestFilesystemTools_NativeSandboxReadsRemainUnrestricted(t *testing.T) {
	fixture := newNativeToolSandbox(t)
	path := filepath.Join(fixture.denied, "readable.txt")
	writeTestFile(t, path, "host data")

	tests := []struct {
		toolID string
		params any
		want   string
	}{
		{
			toolID: "bash",
			params: bashParams{Command: "cat " + quoteShell(path), WorkDir: fixture.workDir},
			want:   "host data",
		},
		{toolID: "read", params: readParams{FilePath: path}, want: "host data"},
		{toolID: "ls", params: LsParams{Path: fixture.denied}, want: "readable.txt"},
		{toolID: "glob", params: globParams{Pattern: "*.txt", Path: fixture.denied}, want: "readable.txt"},
		{toolID: "grep", params: grepParams{Pattern: "host data", Path: path}, want: "host data"},
	}

	for _, tt := range tests {
		t.Run(tt.toolID, func(t *testing.T) {
			impl := fixture.stack.Registry.Get(tt.toolID)
			require.NotNil(t, impl)

			params := marshalToolParams(t, tt.params)
			result, err := impl.Execute(context.Background(), params)
			require.NoError(t, err)
			assert.Contains(t, result.Output, tt.want)
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
	assertTestFileContent(t, allowed, "after")
	assertTestFileContent(t, denied, "before")
}

func newNativeToolSandbox(t *testing.T) nativeToolSandbox {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("bwrap"); err != nil {
			t.Skip("bwrap is not installed")
		}
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
	unified.Tools.Bash.Sandbox.Enabled = true
	unified.Tools.Bash.Sandbox.WritablePaths = []string{configured}
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
