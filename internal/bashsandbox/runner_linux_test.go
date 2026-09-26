//go:build linux

package bashsandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/sandboxpolicy"
)

// requireBubblewrap skips a test on a host without the platform backend.
func requireBubblewrap(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath(bubblewrapExecutable); err != nil {
		t.Skip("bwrap is not installed")
	}
}

// testRunnerFromPolicy builds a live runner for one compiled policy. It creates
// the private temporary backing first: the compiled policy declares that
// directory, but materializing base grants is the session owner's job, so a
// fixture must supply it exactly as production callers do.
func testRunnerFromPolicy(t *testing.T, compiled sandboxpolicy.Policy, workDir, sessionKey string) Runner {
	t.Helper()

	requireBubblewrap(t)
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")

	tempRoot, err := coagenthome.SandboxTempDir(coagenthome.SandboxPathIdentity(compiled.ProjectRoot))
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(tempRoot, 0o700))
	require.NoError(t, os.MkdirAll(filepath.Join(tempRoot, "var-tmp"), 0o700))

	policy, err := buildProcessPolicy(Config{
		Enabled: true, Shields: compiled.Shields, Policy: compiled,
		WorkDir: workDir, SessionKey: sessionKey,
	})
	require.NoError(t, err)

	runner, err := newEnabledRunner(policy)
	require.NoError(t, err)

	return runner
}

// unitRunner builds a bubblewrapRunner for flag-construction tests: no process
// is spawned, so the policy only has to satisfy request validation.
func unitRunner(t *testing.T, project string) *bubblewrapRunner {
	t.Helper()

	policy, err := buildProcessPolicy(Config{
		Enabled: true,
		Policy: sandboxpolicy.Policy{
			ProjectRoot: project,
			Grants: []sandboxpolicy.Grant{{
				Source: "/usr/bin", Target: "/usr/bin",
				Mode: sandboxpolicy.ModeReadOnly, Kind: sandboxpolicy.KindDir, Present: true,
			}},
		},
		WorkDir: project, SessionKey: "unit",
	})
	require.NoError(t, err)

	return &bubblewrapRunner{executable: "/usr/bin/bwrap", policy: policy}
}

func TestBubblewrapRunnerCommandBuildsPrivateNamespace(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")

	project := t.TempDir()
	workDir := filepath.Join(project, "work dir")
	require.NoError(t, os.Mkdir(workDir, 0o755))

	runner := unitRunner(t, project)
	bash, err := filepath.EvalSymlinks("/usr/bin/bash")
	require.NoError(t, err)

	command := "printf '%s\\n' \"$HOME\"; exit 7"
	commandArgs := []string{"hostile ;$()", "line\nbreak", "-leading=equals"}

	cmd, err := runner.Command(t.Context(), procexec.Request{
		Path:    "bash",
		Args:    append([]string{"-c", command}, commandArgs...),
		WorkDir: workDir,
		Env:     []string{"PATH=/usr/bin:/bin", "PWD=/somewhere/else"},
	})
	require.NoError(t, err)

	assert.Equal(t, "/usr/bin/bwrap", cmd.Path)
	assert.Equal(t, "/", cmd.Dir)
	assert.Equal(t, sandboxLauncherEnvironment(), cmd.Env)
	assert.Equal(t, []string{
		"/usr/bin/bwrap",
		"--die-with-parent",
		"--unshare-user",
		"--unshare-pid",
		"--unshare-ipc",
		"--cap-drop", "ALL",
		"--tmpfs", "/",
		"--dir", "/usr",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/dev/shm",
		"--ro-bind-fd", "3", "/usr/bin",
		"--remount-ro", "/",
		"--chdir", workDir,
		"--clearenv",
		"--setenv", "PATH", "/usr/bin:/bin",
		"--setenv", "PWD", workDir,
		"--", bash, "-c", command,
		"hostile ;$()", "line\nbreak", "-leading=equals",
	}, cmd.Args)
	require.Len(t, cmd.ExtraFiles, 1)

	args := cmd.Args[1:]
	assert.NotContains(t, strings.Join(args, " "), "--ro-bind / /", "ordinary mode must not bind the host root")
	assert.NotContains(t, args, "--new-session")
	assert.NotContains(t, args, "--unshare-net")
	assert.NotContains(t, args, "--share-net")
	assert.NotContains(t, args, "--dev-bind")
	assertBubblewrapEnvironment(t, args, "PWD", workDir)
}

func TestBubblewrapRunnerCommandRejectsRequestsOutsideProject(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")

	project := t.TempDir()
	outside := t.TempDir()
	ungranted := filepath.Join(outside, "ungranted-tool")
	require.NoError(t, os.WriteFile(ungranted, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	runner := unitRunner(t, project)

	_, err := runner.Command(t.Context(), procexec.Request{
		Path: "bash", Args: []string{"-c", ":"}, WorkDir: outside,
	})
	require.Error(t, err)
	require.ErrorContains(t, err, "outside the sandboxed project")

	_, err = runner.Command(t.Context(), procexec.Request{
		Path: ungranted, Args: []string{}, WorkDir: project,
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "outside the sandbox's granted paths")
}

func TestBubblewrapRunnerShellCommandMountsSnapshotAtRuntimePath(t *testing.T) {
	project := t.TempDir()
	runner := unitRunner(t, project)
	snapshot := filepath.Join(t.TempDir(), "snap dir", "s")
	require.NoError(t, os.MkdirAll(filepath.Dir(snapshot), 0o700))
	require.NoError(t, os.WriteFile(snapshot, []byte("snapshot"), 0o600))
	runner.provider = fakeProvider{shell: "/bin/bash", snap: snapshot}

	cmd, err := runner.ShellCommand(t.Context(), "go version", project)
	require.NoError(t, err)

	bash, err := filepath.EvalSymlinks("/bin/bash")
	require.NoError(t, err)

	runtimePath := snapshotRuntimePath(snapshot)
	assert.Equal(t, []string{
		"--", bash, "-c", "source '" + runtimePath + "'; go version",
	}, cmd.Args[len(cmd.Args)-4:])

	args := cmd.Args[1:]
	bindIndex := slices.Index(args, runtimePath)
	require.GreaterOrEqual(t, bindIndex, 2)
	assert.Equal(t, "--ro-bind-fd", args[bindIndex-2])
	assert.NotEmpty(t, args[bindIndex-1])
	for _, file := range cmd.ExtraFiles {
		require.NoError(t, file.Close())
	}
}

func TestBubblewrapRunnerShellCommandNoSnapshotUsesBash(t *testing.T) {
	project := t.TempDir()
	runner := unitRunner(t, project) // nil provider

	cmd, err := runner.ShellCommand(t.Context(), "go version", project)
	require.NoError(t, err)

	bash, err := filepath.EvalSymlinks("/usr/bin/bash")
	require.NoError(t, err)
	assert.Equal(t, []string{"--", bash, "-c", "go version"}, cmd.Args[len(cmd.Args)-4:])
	assertBubblewrapEnvironment(t, cmd.Args, "PWD", project)
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
	assert.ErrorContains(t, err, "find Bubblewrap executable")
}

func TestBuildMountPlanOrdersBindsAndMakesNestedHostMountsReadOnly(t *testing.T) {
	plan := buildMountPlan(
		[]sandboxpolicy.Grant{
			{
				Source: "/workspace", Target: "/workspace",
				Mode: sandboxpolicy.ModeReadWrite, Kind: sandboxpolicy.KindDir, Present: true,
			},
			{
				Source: "/workspace/mounted/cache", Target: "/workspace/mounted/cache",
				Mode: sandboxpolicy.ModeReadWrite, Kind: sandboxpolicy.KindDir, Present: true,
			},
			{
				Source: "/var/tmp", Target: "/var/tmp",
				Mode: sandboxpolicy.ModeReadWrite, Kind: sandboxpolicy.KindDir, Present: true,
			},
			{
				Source: "/workspace/absent", Target: "/workspace/absent",
				Mode: sandboxpolicy.ModeReadWrite, Kind: sandboxpolicy.KindDir, Present: false,
			},
		},
		nil,
		[]string{
			"/",
			"/workspace/mounted",
			"/workspace/mounted/cache/nested",
			"/workspace/separate",
			"/unrelated",
		},
	)

	assert.Equal(t, []bindMount{
		{source: "/workspace", target: "/workspace"},
		{source: "/var/tmp", target: "/var/tmp"},
		{source: "/workspace/mounted", target: "/workspace/mounted", readOnly: true},
		{source: "/workspace/separate", target: "/workspace/separate", readOnly: true},
		{source: "/workspace/mounted/cache", target: "/workspace/mounted/cache"},
		{
			source: "/workspace/mounted/cache/nested", target: "/workspace/mounted/cache/nested",
			readOnly: true,
		},
	}, plan.binds)
	assert.Equal(t, []string{"/var", "/workspace", "/workspace/mounted", "/workspace/mounted/cache"}, plan.dirs)
}

func TestBuildMountPlanKeepsExactGrantExplicitlyWritable(t *testing.T) {
	plan := buildMountPlan(
		[]sandboxpolicy.Grant{
			{
				Source: "/workspace", Target: "/workspace",
				Mode: sandboxpolicy.ModeReadWrite, Kind: sandboxpolicy.KindDir, Present: true,
			},
			{
				Source: "/workspace/mounted", Target: "/workspace/mounted",
				Mode: sandboxpolicy.ModeReadWrite, Kind: sandboxpolicy.KindDir, Present: true,
			},
		},
		nil,
		[]string{"/workspace/mounted", "/workspace/mounted/nested"},
	)

	assert.Equal(t, []bindMount{
		{source: "/workspace", target: "/workspace"},
		{source: "/workspace/mounted", target: "/workspace/mounted"},
		{source: "/workspace/mounted/nested", target: "/workspace/mounted/nested", readOnly: true},
	}, plan.binds)
}

func TestBuildMountPlanBindsSocketsAtTheirExactPath(t *testing.T) {
	plan := buildMountPlan(
		nil,
		[]sandboxpolicy.SocketGrant{
			{Path: "/run/user/1000/agent.sock", Present: true},
			{Path: "/run/absent.sock", Present: false},
		},
		nil,
	)

	assert.Equal(t, []bindMount{
		{source: "/run/user/1000/agent.sock", target: "/run/user/1000/agent.sock"},
	}, plan.binds)
}

func TestBuildExtraPlanMountsSnapshotReadOnly(t *testing.T) {
	empty := buildExtraPlan(nil)
	assert.Empty(t, empty.binds)
	assert.Empty(t, empty.dirs)

	absent := buildExtraPlan([]sandboxpolicy.Grant{{Source: "/cache", Target: "/run/x", Present: false}})
	assert.Empty(t, absent.binds)
	assert.Empty(t, absent.dirs)

	plan := buildExtraPlan(snapshotGrant("/cache/snapshot"))
	require.Len(t, plan.binds, 1)
	assert.True(t, plan.binds[0].readOnly)
	assert.Equal(t, snapshotRuntimePath("/cache/snapshot"), plan.binds[0].target)
	assert.Contains(t, plan.dirs, snapshotRuntimeDir)
}

func TestParentDirectoriesListsOrderedAncestors(t *testing.T) {
	assert.Empty(t, parentDirectories(nil))
	assert.Empty(t, parentDirectories([]bindMount{{source: "/a", target: "/"}}))

	dirs := parentDirectories([]bindMount{
		{source: "/x", target: "/a/b/c"},
		{source: "/y", target: "/a/d"},
	})

	assert.Equal(t, []string{"/a", "/a/b"}, dirs)
}

func TestCreateGrantedDirCreatesMissingDirectories(t *testing.T) {
	target := filepath.Join(t.TempDir(), "granted", "nested", "leaf")

	require.NoError(t, createGrantedDir(target))
	assert.DirExists(t, target)

	require.NoError(t, createGrantedDir(target), "an existing directory is accepted unchanged")
}

func TestCreateGrantedDirRefusesSymlinkedComponent(t *testing.T) {
	base := t.TempDir()
	realDir := filepath.Join(base, "real")
	require.NoError(t, os.Mkdir(realDir, 0o700))
	link := filepath.Join(base, "link")
	require.NoError(t, os.Symlink(realDir, link))

	err := createGrantedDir(filepath.Join(link, "nested"))
	require.Error(t, err)
	require.ErrorContains(t, err, "traverses symlink")
	assert.NoDirExists(t, filepath.Join(realDir, "nested"))
}

func TestPinMountPlanHoldsOriginalObjectAcrossReplacement(t *testing.T) {
	parent := t.TempDir()
	source := filepath.Join(parent, "source")
	require.NoError(t, os.Mkdir(source, 0o700))
	plan := mountPlan{binds: []bindMount{{source: source, target: "/granted"}}}
	pinned, files, err := pinMountPlan(plan, 3)
	require.NoError(t, err)
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	require.Len(t, pinned.binds, 1)
	assert.Equal(t, 3, pinned.binds[0].fd)
	require.NoError(t, os.Rename(source, filepath.Join(parent, "moved")))
	require.NoError(t, os.Symlink(t.TempDir(), source))
	_, _, err = pinMountPlan(plan, 3)
	require.Error(t, err, "a replacement symlink cannot become the next bind source")
	info, err := files[0].Stat()
	require.NoError(t, err)
	assert.True(t, info.IsDir(), "the inherited descriptor still names the original directory")
}

func TestCreateGrantedDirRefusesNonDirectoryComponent(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(base, "file")
	require.NoError(t, os.WriteFile(file, []byte("data"), 0o600))

	err := createGrantedDir(filepath.Join(file, "nested"))
	require.Error(t, err)
	assert.ErrorContains(t, err, "non-directory component")
}

func TestMaterializeGrantsCreatesDeclaredWritableDirectoriesOnly(t *testing.T) {
	base := t.TempDir()
	writable := filepath.Join(base, "declared-rw")
	readOnly := filepath.Join(base, "declared-ro")
	present := filepath.Join(base, "present-rw")

	policy := processPolicy{grants: []sandboxpolicy.Grant{
		{Source: writable, Target: writable, Mode: sandboxpolicy.ModeReadWrite, Kind: sandboxpolicy.KindDir},
		{Source: readOnly, Target: readOnly, Mode: sandboxpolicy.ModeReadOnly, Kind: sandboxpolicy.KindDir},
		{
			Source: present, Target: present,
			Mode: sandboxpolicy.ModeReadWrite, Kind: sandboxpolicy.KindDir, Present: true,
		},
	}}

	grants, err := materializeGrants(policy)
	require.NoError(t, err)
	assert.DirExists(t, writable, "an absent read-write declaration is materialized")
	assert.NoDirExists(t, readOnly, "read-only declarations are never created")
	assert.DirExists(t, present, "availability is refreshed before each spawn")

	materialized := grants[0]
	assert.True(t, materialized.Present, "a materialized grant is mountable")
	assert.False(t, grants[1].Present, "a read-only declaration stays unmountable while absent")
}

func TestPolicyOverlapsProcRejectsOverlappingGrants(t *testing.T) {
	procMounts := []string{"/proc", "/proc/sys/fs/binfmt_misc"}

	tests := map[string]struct {
		policy processPolicy
		want   bool
	}{
		"clean policy": {
			policy: processPolicy{projectRoot: "/workspace", workDir: "/workspace"},
		},
		"project under proc": {
			policy: processPolicy{projectRoot: "/proc/self", workDir: "/proc/self"},
			want:   true,
		},
		"work directory under proc": {
			policy: processPolicy{projectRoot: "/workspace", workDir: "/proc/self/cwd"},
			want:   true,
		},
		"grant target under proc": {
			policy: processPolicy{
				projectRoot: "/workspace", workDir: "/workspace",
				grants: []sandboxpolicy.Grant{{Source: "/proc", Target: "/workspace/proc"}},
			},
			want: true,
		},
		"grant source under proc": {
			policy: processPolicy{
				projectRoot: "/workspace", workDir: "/workspace",
				grants: []sandboxpolicy.Grant{{Source: "/proc/sys/fs/binfmt_misc", Target: "/workspace/misc"}},
			},
			want: true,
		},
		"unrelated proc-like prefix": {
			policy: processPolicy{
				projectRoot: "/workspace", workDir: "/workspace",
				grants: []sandboxpolicy.Grant{{Source: "/process", Target: "/workspace/process"}},
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, policyOverlapsProc(tt.policy, procMounts))
		})
	}
}

func TestBubblewrapPrefixBuildsOnePrivateNamespaceForBothShieldStates(t *testing.T) {
	project := t.TempDir()
	plan := buildMountPlan([]sandboxpolicy.Grant{
		{
			Source: project, Target: project,
			Mode: sandboxpolicy.ModeReadWrite, Kind: sandboxpolicy.KindDir, Present: true,
		},
	}, nil, nil)

	ordinary := unitRunner(t, project)
	shielded := unitRunner(t, project)
	shielded.policy.readScope = ProjectConfined

	ordinaryArgs := ordinary.prefix(project, plan, mountPlan{})
	shieldedArgs := shielded.prefix(project, plan, mountPlan{})

	assert.Equal(t, ProjectConfined, ordinary.ReadScope(), "grants, not shields, select the allowlist")
	assert.Equal(t, ProjectConfined, shielded.ReadScope())
	assert.Equal(t, ordinaryArgs, shieldedArgs, "shields are a policy, not a second flag builder")

	for _, args := range [][]string{ordinaryArgs, shieldedArgs} {
		assert.Contains(t, args, "--tmpfs")
		assert.Contains(t, args, "--unshare-pid")
		assert.Contains(t, args, "--proc")
		assert.Contains(t, args, "--remount-ro")
		assert.Contains(t, args, "--dev")
		assert.NotContains(t, strings.Join(args, " "), "--ro-bind / /")
		assert.Equal(t, project, args[len(args)-1], "the flag list ends at the working directory")

		procIndex := slices.Index(args, "/proc")
		require.Positive(t, procIndex)
		assert.Equal(t, "--proc", args[procIndex-1])
		assert.Equal(t, "--dev", args[procIndex+1])

		bindIndex := slices.Index(args, "--bind-fd")
		require.Positive(t, bindIndex)
		assert.Less(t, procIndex, bindIndex, "the fresh proc mount is created before the policy binds")
	}
}

func TestParseMountInfoEntriesParsesAndDeduplicates(t *testing.T) {
	mountInfo := strings.Join([]string{
		"36 29 0:32 / / rw,relatime - overlay overlay rw",
		`37 36 0:33 / /workspace/mounted\040path rw,nosuid - tmpfs tmpfs rw`,
		`38 36 0:34 / /workspace/tab\011newline\012slash\134 rw - tmpfs tmpfs rw`,
		`39 36 0:35 / /workspace/mounted\040path rw - tmpfs tmpfs rw`,
	}, "\n")

	entries, err := parseMountInfoEntries(strings.NewReader(mountInfo))
	require.NoError(t, err)
	assert.Equal(t, []mountInfoEntry{
		{mountPoint: "/", fsType: "overlay"},
		{mountPoint: "/workspace/mounted path", fsType: "tmpfs"},
		{mountPoint: "/workspace/tab\tnewline\nslash\\", fsType: "tmpfs"},
	}, entries)
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

func TestParseMountInfoEntriesRejectsMalformedInput(t *testing.T) {
	tests := map[string]struct {
		mountInfo string
		message   string
	}{
		"truncated escape": {
			mountInfo: `37 36 0:33 / /workspace/bad\04 rw - tmpfs tmpfs rw`,
			message:   "truncated escape",
		},
		"too few fields": {
			mountInfo: "36 29 0:32 /",
			message:   "malformed mountinfo line",
		},
		"missing filesystem type": {
			mountInfo: "36 29 0:32 / /workspace rw,nosuid -",
			message:   "no filesystem type",
		},
		"relative mount point": {
			mountInfo: "36 29 0:32 / workspace rw - tmpfs tmpfs rw",
			message:   "is not absolute",
		},
		"invalid escape digit": {
			mountInfo: `37 36 0:33 / /workspace/bad\999x rw - tmpfs tmpfs rw`,
			message:   "invalid escape",
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := parseMountInfoEntries(strings.NewReader(tt.mountInfo))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.message)
		})
	}
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
	assert.ErrorContains(t, err, "not owned by root")
}

// shellQuote single-quotes a value for safe folding into a `-c` string.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
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
