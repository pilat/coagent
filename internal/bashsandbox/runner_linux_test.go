//go:build linux

package bashsandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

// testRunnerFromPolicy builds a live runner for one compiled policy.
func testRunnerFromPolicy(t *testing.T, compiled sandboxpolicy.Policy, workDir, sessionKey string) Runner {
	t.Helper()

	requireBubblewrap(t)
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")

	policy, err := buildProcessPolicy(Config{
		Enabled: true, Policy: compiled, WorkDir: workDir, SessionKey: sessionKey,
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
			Entries: []sandboxpolicy.Entry{
				{
					Path:    "/",
					Action:  sandboxpolicy.ActionAllow,
					Mode:    sandboxpolicy.ModeReadOnly,
					Kind:    sandboxpolicy.KindDir,
					Present: true,
				},
				{
					Path:    project,
					Action:  sandboxpolicy.ActionAllow,
					Mode:    sandboxpolicy.ModeReadWrite,
					Kind:    sandboxpolicy.KindDir,
					Present: true,
				},
			},
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

	// argv is exercised directly against a fixed mount table: currentMountPlan
	// reads the live host mount table, which varies by machine and would make a
	// golden argv assertion flaky.
	plan, err := buildMountPlan(runner.policy.policy.Entries, []string{"/"})
	require.NoError(t, err)
	plan, mountFiles, err := pinMountPlan(plan)
	require.NoError(t, err)
	t.Cleanup(func() {
		for _, file := range mountFiles {
			_ = file.Close()
		}
	})

	command := "printf '%s\\n' \"$HOME\"; exit 7"
	commandArgs := []string{"hostile ;$()", "line\nbreak", "-leading=equals"}
	requestArgs := append([]string{"-c", command}, commandArgs...)

	environment, err := innerEnvironment([]string{"PATH=/usr/bin:/bin", "PWD=/somewhere/else"}, workDir)
	require.NoError(t, err)

	argv := runner.argv(workDir, bash, requestArgs, environment, plan)

	assert.Equal(t, []string{
		"--die-with-parent",
		"--unshare-user",
		"--unshare-pid",
		"--unshare-ipc",
		"--cap-drop", "ALL",
		"--ro-bind-fd", "3", "/",
		"--bind-fd", "4", project,
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/dev/shm",
		"--chdir", workDir,
		"--clearenv",
		"--setenv", "PATH", "/usr/bin:/bin",
		"--setenv", "PWD", workDir,
		"--", bash, "-c", command,
		"hostile ;$()", "line\nbreak", "-leading=equals",
	}, argv)
	require.Len(t, mountFiles, 2)

	assert.NotContains(t, argv, "--new-session")
	assert.NotContains(t, argv, "--unshare-net")
	assert.NotContains(t, argv, "--share-net")
	assert.NotContains(t, argv, "--dev-bind")
	assertBubblewrapEnvironment(t, argv, "PWD", workDir)
}

func TestBubblewrapRunnerCommandRejectsRequestsOutsideProject(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")

	project := t.TempDir()
	outside := t.TempDir()
	ungranted := filepath.Join(outside, "ungranted-tool")
	require.NoError(t, os.WriteFile(ungranted, []byte("#!/bin/sh\nexit 0\n"), 0o755))

	policy, err := buildProcessPolicy(Config{
		Enabled: true,
		Policy: sandboxpolicy.Policy{
			ProjectRoot: project,
			Entries: []sandboxpolicy.Entry{
				rootEntry(),
				{Path: outside, Action: sandboxpolicy.ActionDeny, Kind: sandboxpolicy.KindDir, Present: true},
				{
					Path:    project,
					Action:  sandboxpolicy.ActionAllow,
					Mode:    sandboxpolicy.ModeReadWrite,
					Kind:    sandboxpolicy.KindDir,
					Present: true,
				},
			},
		},
		WorkDir: project, SessionKey: "unit",
	})
	require.NoError(t, err)
	runner := &bubblewrapRunner{executable: "/usr/bin/bwrap", policy: policy}

	_, err = runner.Command(t.Context(), procexec.Request{
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

func TestBubblewrapRunnerShellCommandSourcesSnapshotThroughInheritedFD(t *testing.T) {
	project := t.TempDir()
	runner := unitRunner(t, project)
	snapshot := filepath.Join(t.TempDir(), "snap dir", "s")
	require.NoError(t, os.MkdirAll(filepath.Dir(snapshot), 0o700))
	require.NoError(t, os.WriteFile(snapshot, []byte("snapshot"), 0o600))
	runner.provider = fakeProvider{shell: "/bin/bash", snap: snapshot}

	cmd, err := runner.ShellCommand(t.Context(), "go version", project)
	require.NoError(t, err)
	defer procexec.CloseExtraFiles(cmd)

	bash, err := filepath.EvalSymlinks("/bin/bash")
	require.NoError(t, err)

	// The snapshot rides in as the last inherited fd, after every mount source.
	fd := 3 + len(cmd.ExtraFiles) - 1
	assert.Equal(t, []string{
		"--", bash, "-c", "source /proc/self/fd/" + strconv.Itoa(fd) + "; go version",
	}, cmd.Args[len(cmd.Args)-4:])
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
		workDir: root, projectRoot: root, writableRoots: []string{root}, sessionKey: "test",
	})
	require.Error(t, err)
	assert.Nil(t, runner)
	assert.ErrorContains(t, err, "find Bubblewrap executable")
}

// opIndex reports where an operation on target appears in the plan, or -1.
func opIndex(plan mountPlan, kind opKind, target string) int {
	for i, op := range plan.ops {
		if op.kind == kind && op.target == target {
			return i
		}
	}

	return -1
}

func planTargets(plan mountPlan, kinds ...opKind) []string {
	want := make(map[opKind]struct{}, len(kinds))
	for _, kind := range kinds {
		want[kind] = struct{}{}
	}

	targets := make([]string, 0, len(plan.ops))

	for _, op := range plan.ops {
		if _, ok := want[op.kind]; ok {
			targets = append(targets, op.target)
		}
	}

	return targets
}

func rootEntry() sandboxpolicy.Entry {
	return sandboxpolicy.Entry{
		Path:    "/",
		Action:  sandboxpolicy.ActionAllow,
		Mode:    sandboxpolicy.ModeReadOnly,
		Kind:    sandboxpolicy.KindDir,
		Present: true,
	}
}

func TestBuildMountPlanRebindsEveryHostMountReadOnly(t *testing.T) {
	entries := []sandboxpolicy.Entry{
		rootEntry(),
		{
			Path:    "/workspace",
			Action:  sandboxpolicy.ActionAllow,
			Mode:    sandboxpolicy.ModeReadWrite,
			Kind:    sandboxpolicy.KindDir,
			Present: true,
		},
	}

	plan, err := buildMountPlan(
		entries, []string{"/", "/home", "/workspace", "/workspace/nested", "/proc", "/proc/sys", "/dev", "/dev/shm"},
	)
	require.NoError(t, err)

	require.NotEmpty(t, plan.ops)
	assert.Equal(t, mountOp{kind: opBindRO, source: "/", target: "/"}, plan.ops[0])

	readOnly := planTargets(plan, opBindRO)
	assert.Contains(t, readOnly, "/home",
		"a nested host mount keeps its own write flag under a read-only root, so it is re-bound")

	assert.NotContains(t, readOnly, "/proc")
	assert.NotContains(t, readOnly, "/proc/sys")
	assert.NotContains(t, readOnly, "/dev")
	assert.NotContains(t, readOnly, "/dev/shm")

	// The workspace entry mounts over its own host mount, since it lands later.
	assert.Contains(t, planTargets(plan, opBindRW), "/workspace")
	assert.Greater(t, opIndex(plan, opBindRW, "/workspace"), opIndex(plan, opBindRO, "/workspace"))
}

func TestBuildMountPlanDenyAfterAllowLandsLater(t *testing.T) {
	home := t.TempDir()
	secret := filepath.Join(home, ".ssh")
	require.NoError(t, os.Mkdir(secret, 0o700))

	entries := []sandboxpolicy.Entry{
		rootEntry(),
		{
			Path:    home,
			Action:  sandboxpolicy.ActionAllow,
			Mode:    sandboxpolicy.ModeReadOnly,
			Kind:    sandboxpolicy.KindDir,
			Present: true,
		},
		{Path: secret, Action: sandboxpolicy.ActionDeny, Kind: sandboxpolicy.KindDir, Present: true},
	}

	plan, err := buildMountPlan(entries, []string{"/", home})
	require.NoError(t, err)

	allow := opIndex(plan, opBindRO, home)
	deny := opIndex(plan, opTmpfs, secret)

	require.NotEqual(t, -1, allow)
	require.NotEqual(t, -1, deny)
	assert.Greater(t, deny, allow, "a deny entry ordered after an allow lands later in the op list")
}

func TestBuildMountPlanLaterRootAllowOverridesDeny(t *testing.T) {
	secret := t.TempDir()
	plan, err := buildMountPlan([]sandboxpolicy.Entry{
		rootEntry(),
		{Path: secret, Action: sandboxpolicy.ActionDeny, Kind: sandboxpolicy.KindDir, Present: true},
		rootEntry(),
	}, []string{"/"})
	require.NoError(t, err)
	deny := opIndex(plan, opTmpfs, secret)
	assert.Equal(t, -1, deny)
	assert.Contains(t, planTargets(plan, opBindRO), "/")
}

func TestBuildMountPlanMasksACredentialFileWithAnEmptyFile(t *testing.T) {
	home := t.TempDir()
	netrc := filepath.Join(home, ".netrc")
	require.NoError(t, os.WriteFile(netrc, []byte("password hunter2"), 0o600))

	entries := []sandboxpolicy.Entry{
		rootEntry(),
		{Path: netrc, Action: sandboxpolicy.ActionDeny, Kind: sandboxpolicy.KindFile, Present: true},
	}

	plan, err := buildMountPlan(entries, []string{"/"})
	require.NoError(t, err)

	assert.NotEqual(t, -1, opIndex(plan, opEmptyFile, netrc),
		"a directory mask cannot cover one file without hiding its siblings")
}

func TestBuildMountPlanRejectsAbsentDenyBeforeCreatingWritablePaths(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing")
	_, err := buildMountPlan([]sandboxpolicy.Entry{
		rootEntry(),
		{Path: path, Action: sandboxpolicy.ActionAllow, Mode: sandboxpolicy.ModeReadWrite, Kind: sandboxpolicy.KindDir},
		{Path: path, Action: sandboxpolicy.ActionDeny, Kind: sandboxpolicy.KindDir},
	}, nil)
	require.ErrorContains(t, err, "does not exist")
	assert.NoDirExists(t, path)
}

func TestBuildMountPlanCreatesAnAbsentWritableDirectoryBeforeBinding(t *testing.T) {
	base := t.TempDir()
	cache := filepath.Join(base, "new-cache")

	entries := []sandboxpolicy.Entry{
		rootEntry(),
		{
			Path:    cache,
			Action:  sandboxpolicy.ActionAllow,
			Mode:    sandboxpolicy.ModeReadWrite,
			Kind:    sandboxpolicy.KindDir,
			Present: false,
		},
	}

	plan, err := buildMountPlan(entries, []string{"/"})
	require.NoError(t, err)

	assert.DirExists(t, cache)
	assert.NotEqual(t, -1, opIndex(plan, opBindRW, cache))
}

func TestBuildMountPlanInstallsAFreshProcAndDevLast(t *testing.T) {
	plan, err := buildMountPlan([]sandboxpolicy.Entry{rootEntry()}, []string{"/", "/proc", "/dev"})
	require.NoError(t, err)

	proc := opIndex(plan, opProc, "/proc")
	dev := opIndex(plan, opDev, devPath)

	require.NotEqual(t, -1, proc)
	require.NotEqual(t, -1, dev)

	for i, op := range plan.ops {
		if op.kind != opBindRO && op.kind != opBindRW {
			continue
		}

		assert.Less(t, i, proc, "a host bind after --proc would shadow the fresh procfs")
		assert.Less(t, i, dev, "a host bind after --dev would shadow the fresh dev")
	}
}

func TestMountArgsTranslatesEveryOperation(t *testing.T) {
	args := mountArgs([]mountOp{
		{kind: opBindRO, target: "/ro", fd: 3},
		{kind: opBindRW, target: "/rw", fd: 4},
		{kind: opEmptyFile, target: "/secret", fd: 5},
		{kind: opTmpfs, target: "/mask"},
		{kind: opProc, target: "/proc"},
		{kind: opDev, target: "/dev"},
	})

	assert.Equal(t, []string{
		"--ro-bind-fd", "3", "/ro",
		"--bind-fd", "4", "/rw",
		"--ro-bind-data", "5", "/secret",
		"--tmpfs", "/mask",
		"--proc", "/proc",
		"--dev", "/dev",
	}, args)
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
	plan := mountPlan{ops: []mountOp{{kind: opBindRW, source: source, target: "/granted"}}}
	pinned, files, err := pinMountPlan(plan)
	require.NoError(t, err)
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	require.Len(t, pinned.ops, 1)
	assert.Equal(t, 3, pinned.ops[0].fd)
	require.NoError(t, os.Rename(source, filepath.Join(parent, "moved")))
	require.NoError(t, os.Symlink(t.TempDir(), source))
	_, _, err = pinMountPlan(plan)
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

func TestPolicyOverlapsProcRejectsOverlappingEntries(t *testing.T) {
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
		"entry under proc": {
			policy: processPolicy{
				projectRoot: "/workspace", workDir: "/workspace",
				policy: sandboxpolicy.Policy{Entries: []sandboxpolicy.Entry{{Path: "/proc/sys/fs/binfmt_misc"}}},
			},
			want: true,
		},
		"unrelated proc-like prefix": {
			policy: processPolicy{
				projectRoot: "/workspace", workDir: "/workspace",
				policy: sandboxpolicy.Policy{Entries: []sandboxpolicy.Entry{{Path: "/process"}}},
			},
		},
	}

	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tt.want, policyOverlapsProc(tt.policy, procMounts))
		})
	}
}

func TestBubblewrapPrefixIsAPureTranslationOfItsPlan(t *testing.T) {
	project := t.TempDir()

	plan, err := buildMountPlan([]sandboxpolicy.Entry{
		rootEntry(),
		{
			Path:    project,
			Action:  sandboxpolicy.ActionAllow,
			Mode:    sandboxpolicy.ModeReadWrite,
			Kind:    sandboxpolicy.KindDir,
			Present: true,
		},
	}, []string{"/"})
	require.NoError(t, err)

	runner := unitRunner(t, project)
	args := runner.prefix(project, plan)

	assert.Equal(t, append([]string{
		"--die-with-parent", "--unshare-user", "--unshare-pid", "--unshare-ipc", "--cap-drop", "ALL",
	}, append(mountArgs(plan.ops), "--chdir", project)...), args)

	assert.Contains(t, args, "--unshare-pid")
	assert.Contains(t, args, "--proc")
	assert.Contains(t, args, "--dev")
	assert.Equal(t, project, args[len(args)-1], "the flag list ends at the working directory")
	assert.Contains(t, strings.Join(args, " "), "--ro-bind-fd 0 /",
		"the launcher starts from the host filesystem, read-only")
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

	executable, err := resolveBubblewrapExecutable(sandboxpolicy.Policy{Entries: []sandboxpolicy.Entry{
		{Path: dir, Action: sandboxpolicy.ActionAllow, Mode: sandboxpolicy.ModeReadWrite},
	}})
	require.NoError(t, err)
	assert.Equal(t, trusted, executable)
}

func TestResolveBubblewrapExecutableRejectsUntrustedTarget(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, bubblewrapExecutable)
	require.NoError(t, os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", dir)

	_, err := resolveBubblewrapExecutable(sandboxpolicy.Policy{})
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
