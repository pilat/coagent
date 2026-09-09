//go:build darwin || linux

package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
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

type activationSentinelProvider struct {
	path string
}

func (p activationSentinelProvider) mark() {
	_ = os.WriteFile(p.path, []byte("activation ran"), 0o600)
}

func (p activationSentinelProvider) Snapshot(context.Context, string) string {
	p.mark()

	return ""
}

func (p activationSentinelProvider) Shell() string {
	p.mark()

	return ""
}

func (p activationSentinelProvider) Fingerprint(string) string {
	p.mark()

	return ""
}

func (p activationSentinelProvider) Invalidate(string) { p.mark() }

func (p activationSentinelProvider) WrapExec(
	ctx context.Context,
	workDir string,
	argv, extraEnv []string,
) (*exec.Cmd, error) {
	p.mark()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = workDir
	cmd.Env = append(os.Environ(), extraEnv...)

	return cmd, nil
}

func (p activationSentinelProvider) LookPath(_ context.Context, _ string, names []string) (string, error) {
	p.mark()

	return exec.LookPath(names[0])
}

func (activationSentinelProvider) Close() error { return nil }

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

func TestFilesystemTools_ShieldedStackDeniesHostReads(t *testing.T) {
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("bwrap"); err != nil {
			t.Skip("bwrap is not installed")
		}
	}

	base := t.TempDir()
	project := filepath.Join(base, "project")
	outside := filepath.Join(base, "outside-secret")
	home := filepath.Join(base, "home")
	require.NoError(t, os.Mkdir(project, 0o755))
	require.NoError(t, os.Mkdir(home, 0o755))
	require.NoError(t, os.WriteFile(outside, []byte("outside"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(home, "home-secret"), []byte("home"), 0o600))
	t.Setenv("HOME", home)
	externalBin := filepath.Join(base, "external-bin")
	require.NoError(t, os.Mkdir(externalBin, 0o755))
	require.NoError(t, os.WriteFile(
		filepath.Join(externalBin, "shield-host-tool"), []byte("#!/bin/sh\nexit 0\n"), 0o755,
	))
	t.Setenv("PATH", externalBin+string(os.PathListSeparator)+os.Getenv("PATH"))

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("network retained"))
	}))
	t.Cleanup(server.Close)
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = writer.Write([]byte("tls retained"))
	}))
	t.Cleanup(tlsServer.Close)
	httpURL := strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	tlsURL := strings.Replace(tlsServer.URL, "127.0.0.1", "localhost", 1)

	unified := &config.UnifiedConfig{}
	unified.Sandbox.Enabled = true
	activationSentinel := filepath.Join(base, "activation-ran")
	stack, err := BuildStack(context.Background(), StackConfig{
		SessionID: 41, WorkDir: project, Unified: unified, ShieldsUp: true,
		Loader: loader.New(), Todo: todo.New(), Provider: activationSentinelProvider{path: activationSentinel},
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, stack.Close()) })
	assert.NoFileExists(t, activationSentinel)

	command := strings.Join([]string{
		"test ! -e " + quoteShell(outside),
		"test ! -e " + quoteShell(filepath.Join(home, "home-secret")),
		"! command -v shield-host-tool",
		"test -r /etc/hosts",
		"test ! -w /tmp",
		"test ! -e /dev/sda",
		"test $(find /proc -maxdepth 1 -type d -name '[0-9]*' | wc -l) -le 8",
		"test \"$(curl --noproxy '*' -fsS " + quoteShell(httpURL) + ")\" = 'network retained'",
		"test \"$(curl --noproxy '*' -kfsS " + quoteShell(tlsURL) + ")\" = 'tls retained'",
	}, " && ")
	result, err := stack.Registry.Get("bash").Execute(
		context.Background(), marshalToolParams(t, bashParams{Command: command, WorkDir: project}),
	)
	require.NoError(t, err)
	assert.Equal(t, 0, result.Metadata[metaKeyExitCode], result.Output)
	assert.NoFileExists(t, activationSentinel)

	nestedWrite := filepath.Join(project, "generated", "write.txt")
	_, err = stack.Registry.Get("write").Execute(context.Background(), marshalToolParams(t, writeParams{
		FilePath: nestedWrite, Content: "written",
	}))
	require.NoError(t, err)
	assertTestFileContent(t, nestedWrite, "written")

	nestedPatch := filepath.Join(project, "patched", "new.txt")
	patch := `--- /dev/null
+++ b/patched/new.txt
@@ -0,0 +1,1 @@
+patched`
	_, err = stack.Registry.Get("apply_patch").Execute(
		context.Background(), marshalApplyPatchParams(t, patch),
	)
	require.NoError(t, err)
	assertTestFileContent(t, nestedPatch, "patched")

	writeTestFile(t, filepath.Join(project, "inside.txt"), "inside")
	require.NoError(t, os.Symlink(outside, filepath.Join(project, "outside-link")))
	grepResult, err := stack.Registry.Get("grep").Execute(context.Background(), marshalToolParams(t, grepParams{
		Pattern: "outside", Path: project,
	}))
	require.NoError(t, err)
	assert.NotContains(t, grepResult.Output, outside)
	for _, call := range []struct {
		toolID string
		params any
	}{
		{toolID: "read", params: readParams{FilePath: outside}},
		{toolID: "ls", params: LsParams{Path: base}},
		{toolID: "glob", params: globParams{Pattern: "*", Path: base}},
		{toolID: "grep", params: grepParams{Pattern: "outside", Path: outside}},
		{toolID: "write", params: writeParams{FilePath: outside, Content: "changed"}},
		{toolID: "edit", params: editParams{FilePath: outside, OldString: "outside", NewString: "changed"}},
	} {
		t.Run("denies_"+call.toolID, func(t *testing.T) {
			_, err := stack.Registry.Get(call.toolID).Execute(context.Background(), marshalToolParams(t, call.params))
			require.Error(t, err)
			assert.ErrorContains(t, err, "Coagent shields are raised; filesystem access is confined to the project.")
		})
	}

	outsidePatch := fmt.Sprintf("--- %[1]s\n+++ %[1]s\n@@ -1,1 +1,1 @@\n-outside\n+changed", outside)
	_, err = stack.Registry.Get("apply_patch").Execute(
		context.Background(), marshalApplyPatchParams(t, outsidePatch),
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "Coagent shields are raised; filesystem access is confined to the project.")
	assertTestFileContent(t, outside, "outside")
}

func TestSandboxProcessArtifactShieldBoundary(t *testing.T) {
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("bwrap"); err != nil {
			t.Skip("bwrap is not installed")
		}
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	project := t.TempDir()
	processRoot, err := coagenthome.ProcessProjectDir(7)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(processRoot, 0o700))
	artifact := filepath.Join(processRoot, "1", "artifact.output")
	writeTestFile(t, artifact, "before\nlast line\n")

	unified := &config.UnifiedConfig{}
	unified.Sandbox.Enabled = true
	ordinary, err := BuildStack(context.Background(), StackConfig{
		ProjectID: 7, SessionID: 1, WorkDir: project, Unified: unified,
		Loader: loader.New(), Todo: todo.New(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, ordinary.Close()) })

	for _, toolID := range []string{"read", "tail"} {
		params := any(readParams{FilePath: artifact})
		if toolID == "tail" {
			params = tailParams{FilePath: artifact}
		}
		result, executeErr := ordinary.Registry.Get(toolID).Execute(
			context.Background(), marshalToolParams(t, params),
		)
		require.NoError(t, executeErr)
		assert.Contains(t, result.Output, "last line")
	}
	require.NoError(t, executeToolMutation(t, ordinary.Registry.Get("write"), artifact))
	assertTestFileContent(t, artifact, "after")

	shielded, err := BuildStack(context.Background(), StackConfig{
		ProjectID: 7, SessionID: 1, WorkDir: project, Unified: unified, ShieldsUp: true,
		Loader: loader.New(), Todo: todo.New(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, shielded.Close()) })
	for _, toolID := range []string{"read", "tail", "write"} {
		impl := shielded.Registry.Get(toolID)
		require.NotNil(t, impl)
		var executeErr error
		if toolID == "write" {
			executeErr = executeToolMutation(t, impl, artifact)
		} else {
			params := any(readParams{FilePath: artifact})
			if toolID == "tail" {
				params = tailParams{FilePath: artifact}
			}
			_, executeErr = impl.Execute(context.Background(), marshalToolParams(t, params))
		}
		require.Error(t, executeErr, "%s must reject an external process artifact under shields", toolID)
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
	unified.Sandbox.Enabled = true
	unified.Sandbox.WritablePaths = []string{configured}
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
