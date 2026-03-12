//go:build linux

package bashsandbox

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBubblewrapRunnerCommand(t *testing.T) {
	runner := &bubblewrapRunner{
		executable: "/usr/bin/bwrap",
		mounts: []mountOperation{
			{path: "/tmp/root"},
			{path: "/tmp/root with spaces"},
			{path: "/tmp/-leading=equals"},
			{path: "/tmp/root/child", readOnly: true},
		},
	}
	command := "printf '%s\\n' \"$HOME\"; exit 7"
	commandArgs := []string{"hostile ;$()", "line\nbreak", "-leading=equals"}

	cmd, err := runner.Command(context.Background(), command, "/tmp/work dir", commandArgs...)
	require.NoError(t, err)
	assert.Equal(t, "/usr/bin/bwrap", cmd.Path)
	assert.Equal(t, "/tmp/work dir", cmd.Dir)
	assert.Equal(t, []string{
		"--unshare-user",
		"--cap-drop", "ALL",
		"--",
		"bash", "-c", command,
		"hostile ;$()", "line\nbreak", "-leading=equals",
	}, cmd.Args[len(cmd.Args)-10:])

	args := cmd.Args[1:]
	assert.Equal(t, "--die-with-parent", args[0])
	assert.Equal(t, []string{"--ro-bind", "/", "/"}, args[1:4])
	assert.Equal(t, []string{"--dev", "/dev"}, args[4:6])
	assert.NotContains(t, args, "--new-session")
	assert.NotContains(t, args, "--unshare-pid")
	assert.NotContains(t, args, "--chdir")
	assert.NotContains(t, args, "--unshare-net")
	assert.NotContains(t, args, "--unshare-all")
	assert.NotContains(t, args, "--share-net")
	assert.NotContains(t, args, "--dev-bind")
	assert.Contains(t, args, "--unshare-user")
	assert.Contains(t, args, "--cap-drop")

	assertMountPair(t, args, "--ro-bind", "/tmp/root/child")
	assertBindPair(t, args, "/tmp/root with spaces")
	assertBindPair(t, args, "/tmp/-leading=equals")
	assertBindPair(t, args, "/tmp/root")
}

func TestBubblewrapRunnerShellCommand(t *testing.T) {
	runner := &bubblewrapRunner{
		executable: "/usr/bin/bwrap",
		mounts:     []mountOperation{{path: "/tmp/root"}},
		provider:   fakeProvider{shell: "/bin/bash", snap: "/tmp/snap dir/s"},
	}

	cmd, err := runner.ShellCommand(context.Background(), "go version", "/tmp/work")
	require.NoError(t, err)

	assert.Equal(t, "/tmp/work", cmd.Dir)
	assert.Equal(t, []string{
		"--unshare-user", "--cap-drop", "ALL", "--",
		"/bin/bash", "-c", "source '/tmp/snap dir/s'; go version",
	}, cmd.Args[len(cmd.Args)-7:])
}

func TestBubblewrapRunnerShellCommandNoSnapshotUsesBash(t *testing.T) {
	runner := &bubblewrapRunner{executable: "/usr/bin/bwrap"} // nil provider

	cmd, err := runner.ShellCommand(context.Background(), "go version", "/tmp/work")
	require.NoError(t, err)

	assert.Equal(t, []string{
		"--unshare-user", "--cap-drop", "ALL", "--",
		"bash", "-c", "go version",
	}, cmd.Args[len(cmd.Args)-7:])
}

func TestNewEnabledRunnerRequiresBubblewrap(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	runner, err := newEnabledRunner([]string{t.TempDir()})
	require.Error(t, err)
	assert.Nil(t, runner)
	assert.Contains(t, err.Error(), "find Bubblewrap executable")
}

func TestBuildMountOperationsProtectsNestedMounts(t *testing.T) {
	operations := buildMountOperations(
		[]string{"/workspace", "/workspace/mounted/cache", "/var/tmp"},
		[]string{
			"/",
			"/workspace/mounted",
			"/workspace/mounted/cache/nested",
			"/workspace/separate",
			"/unrelated",
		},
	)

	assert.Equal(t, []mountOperation{
		{path: "/workspace"},
		{path: "/var/tmp"},
		{path: "/workspace/mounted", readOnly: true},
		{path: "/workspace/separate", readOnly: true},
		{path: "/workspace/mounted/cache"},
		{path: "/workspace/mounted/cache/nested", readOnly: true},
	}, operations)
}

func TestBuildMountOperationsKeepsExactMountExplicitlyWritable(t *testing.T) {
	operations := buildMountOperations(
		[]string{"/workspace", "/workspace/mounted"},
		[]string{"/workspace/mounted", "/workspace/mounted/nested"},
	)

	assert.Equal(t, []mountOperation{
		{path: "/workspace"},
		{path: "/workspace/mounted"},
		{path: "/workspace/mounted/nested", readOnly: true},
	}, operations)
}

func TestParseMountInfo(t *testing.T) {
	mountInfo := strings.Join([]string{
		"36 29 0:32 / / rw,relatime - overlay overlay rw",
		`37 36 0:33 / /workspace/mounted\040path rw,nosuid - tmpfs tmpfs rw`,
		`38 36 0:34 / /workspace/tab\011newline\012slash\134 rw - tmpfs tmpfs rw`,
		`39 36 0:35 / /workspace/mounted\040path rw - tmpfs tmpfs rw`,
	}, "\n")

	mountPoints, err := parseMountInfo(strings.NewReader(mountInfo))
	require.NoError(t, err)
	assert.Equal(t, []string{
		"/",
		"/workspace/mounted path",
		"/workspace/tab\tnewline\nslash\\",
	}, mountPoints)
}

func TestParseMountInfoRejectsMalformedEscape(t *testing.T) {
	_, err := parseMountInfo(strings.NewReader(`37 36 0:33 / /workspace/bad\04 rw - tmpfs tmpfs rw`))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "truncated escape")
}

func TestValidateBubblewrapExecutable(t *testing.T) {
	tests := map[string]struct {
		mode    os.FileMode
		uid     uint32
		roots   []string
		message string
	}{
		"root owned immutable": {mode: 0o755, uid: 0},
		"not regular":          {mode: os.ModeDir | 0o755, uid: 0, message: "not an executable regular file"},
		"not executable":       {mode: 0o644, uid: 0, message: "not an executable regular file"},
		"not root owned":       {mode: 0o755, uid: 1000, message: "not owned by root"},
		"group writable":       {mode: 0o775, uid: 0, message: "group- or world-writable"},
		"world writable":       {mode: 0o757, uid: 0, message: "group- or world-writable"},
		"under writable root": {
			mode: 0o755, uid: 0, roots: []string{"/nix/store/package"}, message: "under writable root",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateBubblewrapExecutable("/nix/store/package/bin/bwrap", tt.mode, tt.uid, tt.roots)
			if tt.message == "" {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.message)
		})
	}
}

func TestResolveBubblewrapExecutableCanonicalizesTrustedSymlink(t *testing.T) {
	trusted, err := filepath.EvalSymlinks("/bin/true")
	require.NoError(t, err)

	dir := t.TempDir()
	require.NoError(t, os.Symlink(trusted, filepath.Join(dir, bubblewrapExecutable)))
	t.Setenv("PATH", dir)

	executable, err := resolveBubblewrapExecutable([]string{dir})
	require.NoError(t, err)
	assert.Equal(t, trusted, executable)
}

func TestResolveBubblewrapExecutableRejectsUntrustedTarget(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, bubblewrapExecutable)
	require.NoError(t, os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", dir)

	_, err := resolveBubblewrapExecutable(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not owned by root")
}

func assertBindPair(t *testing.T, args []string, root string) {
	t.Helper()

	assertMountPair(t, args, "--bind", root)
}

func assertMountPair(t *testing.T, args []string, operation, root string) {
	t.Helper()

	index := slices.Index(args, root)
	require.Positive(t, index)
	assert.Equal(t, operation, args[index-1])
	require.Less(t, index+1, len(args))
	assert.Equal(t, root, args[index+1])
}
