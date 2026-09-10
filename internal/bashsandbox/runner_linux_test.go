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

	cmd, err := runner.BashCommand(context.Background(), command, "/tmp/work dir", commandArgs...)
	require.NoError(t, err)
	assert.Equal(t, "/usr/bin/bwrap", cmd.Path)
	assert.Equal(t, "/tmp/work dir", cmd.Dir)
	assert.Equal(t, []string{
		"--", "bash", "-c", command, "hostile ;$()", "line\nbreak", "-leading=equals",
	}, cmd.Args[len(cmd.Args)-7:])

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
	assert.Contains(t, args, "--clearenv")
	assertBubblewrapEnvironment(t, args, "PWD", "/tmp/work dir")

	assertMountPair(t, args, "--ro-bind", "/tmp/root/child")
	assertBindPair(t, args, "/tmp/root with spaces")
	assertBindPair(t, args, "/tmp/-leading=equals")
	assertBindPair(t, args, "/tmp/root")
	procIndex := slices.Index(args, "/proc")
	require.Positive(t, procIndex)
	assert.Equal(t, "--proc", args[procIndex-1])
	assert.Equal(t, "--unshare-user", args[procIndex+1])
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
		"--", "/bin/bash", "-c", "source '/tmp/snap dir/s'; go version",
	}, cmd.Args[len(cmd.Args)-4:])
	assertBubblewrapEnvironment(t, cmd.Args, "PWD", "/tmp/work")
}

func TestBubblewrapRunnerShellCommandNoSnapshotUsesBash(t *testing.T) {
	runner := &bubblewrapRunner{executable: "/usr/bin/bwrap"} // nil provider

	cmd, err := runner.ShellCommand(context.Background(), "go version", "/tmp/work")
	require.NoError(t, err)

	assert.Equal(t, []string{"--", "bash", "-c", "go version"}, cmd.Args[len(cmd.Args)-4:])
	assertBubblewrapEnvironment(t, cmd.Args, "PWD", "/tmp/work")
}

func assertBubblewrapEnvironment(t *testing.T, args []string, name, value string) {
	t.Helper()
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "--setenv" && args[i+1] == name && args[i+2] == value {
			return
		}
	}
	t.Fatalf("missing --setenv %s %s in %q", name, value, args)
}

func TestNewEnabledRunnerRequiresBubblewrap(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	root := t.TempDir()
	runner, err := newEnabledRunner(processPolicy{
		readScope: HostReadable, workDir: root, projectRoot: root,
		writableRoots: []string{root}, sessionKey: "test",
	})
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

func TestBuildShieldMountOperationsProtectsNestedProjectMounts(t *testing.T) {
	policy := processPolicy{
		readScope: ProjectConfined, projectRoot: "/canonical/project", workDir: "/visible/project",
		readMounts: []policyMount{{source: "/usr/bin", target: "/usr/bin", directory: true}},
	}

	operations := buildShieldMountOperations(policy, []string{
		"/canonical/project/nested", "/usr/bin/separate-mount",
	}, nil)

	assert.Equal(t, []shieldMountOperation{
		{source: "/canonical/project", target: "/canonical/project"},
		{source: "/usr/bin", target: "/usr/bin", readOnly: true},
		{source: "/canonical/project", target: "/visible/project"},
		{
			source: "/canonical/project/nested", target: "/canonical/project/nested", readOnly: true,
		},
		{
			source: "/usr/bin/separate-mount", target: "/usr/bin/separate-mount", readOnly: true,
		},
		{
			source: "/canonical/project/nested", target: "/visible/project/nested", readOnly: true,
		},
	}, operations)
}

func TestBuildShieldMountOperationsSkipsProcMounts(t *testing.T) {
	policy := processPolicy{
		readScope: ProjectConfined, projectRoot: "/canonical/project", workDir: "/visible/project",
		readMounts: []policyMount{{source: "/usr/bin", target: "/usr/bin", directory: true}},
	}

	operations := buildShieldMountOperations(policy,
		[]string{"/canonical/project/proc", "/usr/bin/proc"},
		[]string{"/canonical/project/proc", "/usr/bin/proc"},
	)

	assert.Empty(t, operations)
}

func TestBubblewrapShieldPrefixBuildsEmptyNamespace(t *testing.T) {
	runner := &bubblewrapRunner{
		policy: processPolicy{readScope: ProjectConfined},
		shieldMounts: []shieldMountOperation{
			{source: "/project", target: "/project"},
			{source: "/usr/bin", target: "/usr/bin", readOnly: true},
		},
	}

	args := runner.shieldPrefix("/project")
	assert.Contains(t, args, "--tmpfs")
	assert.Contains(t, args, "--unshare-pid")
	assert.Contains(t, args, "--proc")
	assert.Contains(t, args, "--remount-ro")
	procIndex := slices.Index(args, "/proc")
	bindIndex := slices.Index(args, "--bind")
	require.Positive(t, procIndex)
	require.Positive(t, bindIndex)
	assert.Greater(t, procIndex, bindIndex)
	assert.NotContains(t, args, "/tmp")
	assert.NotContains(t, strings.Join(args, " "), "--ro-bind / /")
}

func TestNormalizeWritableRootRejectsLinuxSpecialRoots(t *testing.T) {
	for _, path := range []string{"/proc", "/dev", "/sys"} {
		t.Run(path, func(t *testing.T) {
			_, err := normalizeWritableRoot(path)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "cannot be under protected Linux root")
		})
	}
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

func TestParseMountInfoEntriesRetainsProcOnDuplicateMountPoint(t *testing.T) {
	mountInfo := strings.Join([]string{
		"36 29 0:32 / /workspace rw - tmpfs tmpfs rw",
		"37 36 0:33 / /workspace rw - proc proc rw",
	}, "\n")

	entries, err := parseMountInfoEntries(strings.NewReader(mountInfo))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "/workspace", entries[0].mountPoint)
	assert.Equal(t, "proc", entries[0].fsType)
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
		nested  bool
		roots   []string
		message string
	}{
		"root owned immutable":           {mode: 0o755, uid: 0},
		"not regular":                    {mode: os.ModeDir | 0o755, uid: 0, message: "not an executable regular file"},
		"not executable":                 {mode: 0o644, uid: 0, message: "not an executable regular file"},
		"not root owned":                 {mode: 0o755, uid: 1000, message: "not owned by root"},
		"root owner in nested namespace": {mode: 0o755, uid: 0, nested: true, message: "not owned by root"},
		"group writable":                 {mode: 0o775, uid: 0, message: "group- or world-writable"},
		"world writable":                 {mode: 0o757, uid: 0, message: "group- or world-writable"},
		"under writable root": {
			mode: 0o755, uid: 0, roots: []string{"/nix/store/package"}, message: "under writable root",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			err := validateBubblewrapExecutable("/nix/store/package/bin/bwrap", tt.mode, tt.uid, tt.nested, tt.roots)
			if tt.message == "" {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.message)
		})
	}
}

func TestTrustedUnmappedRootOwner(t *testing.T) {
	const overflowUID = uint32(65534)

	tests := map[string]struct {
		executable string
		uid        uint32
		nested     bool
		readOnly   bool
		roots      []string
		want       bool
	}{
		"nested read-only system path": {
			executable: "/usr/bin/bwrap",
			uid:        overflowUID,
			nested:     true,
			readOnly:   true,
			roots:      []string{"/workspace"},
			want:       true,
		},
		"initial namespace": {
			executable: "/usr/bin/bwrap",
			uid:        overflowUID,
			nested:     false,
			readOnly:   true,
			want:       false,
		},
		"writable mount": {
			executable: "/usr/bin/bwrap",
			uid:        overflowUID,
			nested:     true,
			readOnly:   false,
			want:       false,
		},
		"writable root": {
			executable: "/workspace/bwrap",
			uid:        overflowUID,
			nested:     true,
			readOnly:   true,
			roots:      []string{"/workspace"},
			want:       false,
		},
		"different owner": {
			executable: "/usr/bin/bwrap",
			uid:        1000,
			nested:     true,
			readOnly:   true,
			want:       false,
		},
		"zero overflow owner": {
			executable: "/usr/bin/bwrap",
			uid:        0,
			nested:     true,
			readOnly:   true,
			want:       false,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, trustedUnmappedRootOwnerWith(
				tt.executable,
				tt.uid,
				overflowUID,
				tt.nested,
				tt.readOnly,
				tt.roots,
			))
		})
	}
}

func TestParseUserNamespaceMap(t *testing.T) {
	tests := map[string]struct {
		uidMap    string
		want      bool
		wantError bool
	}{
		"initial": {
			uidMap: "0 0 4294967295\n",
			want:   false,
		},
		"rootless": {
			uidMap: "1000 0 1\n",
			want:   true,
		},
		"multiple mappings": {
			uidMap: "0 1000 1\n1 1001 1\n",
			want:   true,
		},
		"empty": {
			uidMap:    "",
			wantError: true,
		},
		"malformed field count": {
			uidMap:    "0 0\n",
			wantError: true,
		},
		"malformed number": {
			uidMap:    "0 nope 1\n",
			wantError: true,
		},
		"zero length": {
			uidMap:    "0 0 0\n",
			wantError: true,
		},
		"range overflow": {
			uidMap:    "0 0 4294967296\n",
			wantError: true,
		},
		"overlap": {
			uidMap:    "0 1000 2\n1 2000 1\n",
			wantError: true,
		},
		"too many extents": {
			uidMap:    "0 1000 1\n1 1001 1\n2 1002 1\n3 1003 1\n4 1004 1\n5 1005 1\n",
			wantError: true,
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			nested, err := parseUserNamespaceMap(tt.uidMap)
			if tt.wantError {
				require.Error(t, err)
				return
			}

			require.NoError(t, err)
			assert.Equal(t, tt.want, nested)
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
