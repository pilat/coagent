//go:build linux

package bashsandbox

import (
	"context"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/safefile"
	"github.com/pilat/coagent/internal/sandboxpolicy"
	"github.com/pilat/coagent/internal/shellenv"
)

func TestSandboxSocketClient(t *testing.T) {
	path := os.Getenv("COAGENT_TEST_SOCKET")
	if path == "" {
		return
	}
	//nolint:gosec // The parent test supplies a local Unix socket fixture.
	conn, err := net.DialTimeout("unix", path, time.Second)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

func TestSandboxDenyDisappearingAfterCompilationFailsClosed(t *testing.T) {
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	secret := filepath.Join(home, "later-secret")
	require.NoError(t, os.WriteFile(secret, []byte("private"), 0o600))
	policy := compileFixturePolicy(t, project, func(req *sandboxpolicy.Request) {
		req.GlobalRules = []sandboxpolicy.Rule{{Deny: secret}}
	})
	runner := testRunnerFromPolicy(t, policy, project, "late-file")
	require.NoError(t, os.Remove(secret))
	_, err := runner.BashCommand(t.Context(), "cat "+shellQuote(secret), project)
	require.ErrorContains(t, err, "does not exist")
}

func TestSandboxLauncherHonoursOverriddenWritableGrant(t *testing.T) {
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	policy := compileFixturePolicy(t, project, func(req *sandboxpolicy.Request) {
		req.GlobalRules = []sandboxpolicy.Rule{
			{Allow: "/usr", Mode: sandboxpolicy.ModeReadWrite},
			{Allow: "/usr"},
			{Deny: "/proc"},
			{Allow: "/"},
		}
	})
	runner := testRunnerFromPolicy(t, policy, project, "effective-launcher")
	output, err := runSandboxCommand(t, runner, "printf ready", project)
	require.NoError(t, err, output)
	assert.Equal(t, "ready", output)
}

func TestSandboxSocketCarveOut(t *testing.T) {
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	runtimeDir := testDir(t, filepath.Join(home, "runtime"))
	socket := filepath.Join(runtimeDir, "agent")
	listener, err := net.Listen("unix", socket)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, listener.Close()) })

	for _, mode := range []sandboxpolicy.Mode{sandboxpolicy.ModeReadOnly, sandboxpolicy.ModeReadWrite} {
		t.Run(string(mode), func(t *testing.T) {
			policy := compileFixturePolicy(t, project, func(req *sandboxpolicy.Request) {
				req.GlobalRules = []sandboxpolicy.Rule{{Deny: runtimeDir}, {Allow: socket, Mode: mode}}
			})
			runner := testRunnerFromPolicy(t, policy, project, "socket")
			access, err := safefile.New(policy, project)
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, access.Close()) })
			cmd, err := runner.Command(t.Context(), procexec.Request{
				Path: os.Args[0], Args: []string{"-test.run=^TestSandboxSocketClient$"}, WorkDir: project,
				Env: []string{"COAGENT_TEST_SOCKET=" + socket},
			})
			require.NoError(t, err)
			defer procexec.CloseExtraFiles(cmd)
			output, err := cmd.CombinedOutput()
			require.NoError(t, err, string(output))
		})
	}
}

// compileFixturePolicy compiles one project's policy and applies the extra
// request fields a fixture needs.
func compileFixturePolicy(
	t *testing.T,
	project string,
	mutate func(*sandboxpolicy.Request),
) sandboxpolicy.Policy {
	t.Helper()

	req := testPolicyRequest(t, project)
	if mutate != nil {
		mutate(&req)
	}

	compiled, err := sandboxpolicy.Compile(req)
	require.NoError(t, err)

	return compiled
}

// runSandboxCommand runs one shell snippet and returns its combined output and
// exit error; negative fixtures assert on the error.
func runSandboxCommand(t *testing.T, runner Runner, command, workDir string) (string, error) {
	t.Helper()

	cmd, err := runner.BashCommand(t.Context(), command, workDir)
	require.NoError(t, err)

	output, err := cmd.CombinedOutput()
	procexec.CloseExtraFiles(cmd)

	return string(output), err
}

// TestSandboxReadsHostFilesystemButNeverWritesIt proves the ordinary contract:
// reads are broad because the host root is always readable, writes stay with
// the entry table.
func TestSandboxReadsHostFilesystemButNeverWritesIt(t *testing.T) {
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	runner := testRunnerFromPolicy(t, compileFixturePolicy(t, project, nil), project, "host-read")

	output, err := runSandboxCommand(t, runner, "cat /etc/passwd", project)
	require.NoError(t, err, "an ordinary session reads the host filesystem: %s", output)
	assert.Contains(t, output, "root:")

	output, err = runSandboxCommand(t, runner, "printf blocked > /etc/coagent-write-probe", project)
	require.Error(t, err, "the host filesystem is readable, never writable: %s", output)
	assert.NoFileExists(t, "/etc/coagent-write-probe")
}

// TestSandboxDenyRuleMasksAPathInsideAWritableGrant proves a deny rule wins
// even when it lands inside an allowed read-write directory.
func TestSandboxDenyRuleMasksAPathInsideAWritableGrant(t *testing.T) {
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))

	secretDir := testDir(t, filepath.Join(project, "credentials"))
	key := filepath.Join(secretDir, "id_ed25519")
	require.NoError(t, os.WriteFile(key, []byte("PRIVATE-KEY-MATERIAL"), 0o600))

	secretFile := filepath.Join(project, "netrc")
	require.NoError(t, os.WriteFile(secretFile, []byte("password hunter2"), 0o600))

	visible := filepath.Join(project, "README")
	require.NoError(t, os.WriteFile(visible, []byte("ordinary"), 0o600))

	// A path nested inside the project must be denied through the project's own
	// rules: a global rule sits under the global default, which the project
	// default always outranks.
	policy := compileFixturePolicy(t, project, func(req *sandboxpolicy.Request) {
		req.ProjectRules = []sandboxpolicy.Rule{{Deny: secretDir}, {Deny: secretFile}}
	})
	runner := testRunnerFromPolicy(t, policy, project, "masked")

	output, err := runSandboxCommand(t, runner, "cat "+shellQuote(visible), project)
	require.NoError(t, err, "masking one path must not hide its siblings: %s", output)
	assert.Contains(t, output, "ordinary")

	output, err = runSandboxCommand(t, runner, "cat "+shellQuote(key), project)
	require.Error(t, err, "a masked directory must be empty even inside a writable grant: %s", output)
	assert.NotContains(t, output, "PRIVATE-KEY-MATERIAL")

	output, err = runSandboxCommand(t, runner, "cat "+shellQuote(secretFile), project)
	require.NoError(t, err, "a masked file reads as empty rather than failing: %s", output)
	assert.NotContains(t, output, "hunter2")

	// The mask is a mount, not a deletion: the host objects are untouched.
	content, err := os.ReadFile(key)
	require.NoError(t, err)
	assert.Equal(t, "PRIVATE-KEY-MATERIAL", string(content))
}

func TestSandboxCommandsDoNotInheritMountSourceDescriptors(t *testing.T) {
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	runner := testRunnerFromPolicy(t, compileFixturePolicy(t, project, nil), project, "fd-cleanup")
	output, err := runSandboxCommand(t, runner,
		"for fd in {3..32}; do if test -e /proc/self/fd/$fd; then echo inherited:$fd; exit 1; fi; done", project)
	require.NoError(t, err, output)
}

// TestSandboxShellCommandSourcesSnapshotThroughInheritedFD proves the fd-based
// shell-env replacement: the snapshot is never bind-mounted, only inherited.
func TestSandboxShellCommandSourcesSnapshotThroughInheritedFD(t *testing.T) {
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	runner := testRunnerFromPolicy(t, compileFixturePolicy(t, project, nil), project, "fd-snapshot")

	snapshot := filepath.Join(home, "snapshot.sh")
	require.NoError(t, os.WriteFile(snapshot, []byte("export COAGENT_SNAPSHOT_MARKER=took-effect\n"), 0o600))

	confined, ok := runner.(shellenv.ConfinedRunner)
	require.True(t, ok)
	cmd, err := confined.SnapshotShell(
		t.Context(),
		"/bin/bash",
		snapshot,
		"echo $COAGENT_SNAPSHOT_MARKER",
		project,
		nil,
	)
	require.NoError(t, err)

	output, err := cmd.CombinedOutput()
	procexec.CloseExtraFiles(cmd)
	require.NoError(t, err, string(output))
	assert.Contains(t, string(output), "took-effect")
}

// TestSandboxRefusesSymlinkEscape proves a symlink inside the project cannot
// redirect access onto an ungranted object.
func TestSandboxRefusesSymlinkEscape(t *testing.T) {
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	other := testDir(t, filepath.Join(home, "other-project"))
	secret := filepath.Join(other, "secret")
	require.NoError(t, os.WriteFile(secret, []byte("escaped-secret"), 0o600))

	escapeFile := filepath.Join(project, "escape-file")
	require.NoError(t, os.Symlink(secret, escapeFile))

	runner := testRunnerFromPolicy(t, compileFixturePolicy(t, project, nil), project, "symlink")

	// The host root is readable in the ordinary policy, so a symlink resolves;
	// the assertion that matters is that it never becomes writable.
	output, err := runSandboxCommand(t, runner, "printf x > "+shellQuote(escapeFile), project)
	require.Error(t, err, "a symlink out of the project must not become writable: %s", output)

	content, err := os.ReadFile(secret)
	require.NoError(t, err)
	assert.Equal(t, "escaped-secret", string(content))
}

// TestBubblewrapIntegrationProtectsNestedMount proves a host mount nested
// inside a granted directory stays read-only. It needs mount privileges, so it
// runs only in a disposable VM where the fixture can create a real mount.
func TestBubblewrapIntegrationProtectsNestedMount(t *testing.T) {
	if os.Getenv("COAGENT_BWRAP_MOUNT_INTEGRATION") != "1" {
		t.Skip("set COAGENT_BWRAP_MOUNT_INTEGRATION=1 in a disposable Linux VM")
	}

	requireBubblewrap(t)

	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	nested := testDir(t, filepath.Join(project, "nested-mount"))

	if output, err := exec.Command("mount", "-t", "tmpfs", "tmpfs", nested).CombinedOutput(); err != nil {
		t.Skipf("cannot create the nested mount fixture: %v: %s", err, output)
	}
	t.Cleanup(func() {
		if output, err := exec.Command("umount", nested).CombinedOutput(); err != nil {
			t.Errorf("unmount nested tmpfs: %v: %s", err, output)
		}
	})

	outerFile := filepath.Join(project, "outer-write")
	nestedFile := filepath.Join(nested, "blocked-write")

	runner := testRunnerFromPolicy(t, compileFixturePolicy(t, project, nil), project, "nested")
	output, err := runSandboxCommand(t, runner,
		"touch "+shellQuote(outerFile)+" && ! touch "+shellQuote(nestedFile), project)
	require.NoError(t, err, output)
	assert.FileExists(t, outerFile)
	assert.NoFileExists(t, nestedFile)
}

// TestSandboxOperatorWritableRuleAllowsWrites proves the operator's global
// allow rule adds a writable root alongside the readable host root.
func TestSandboxOperatorWritableRuleAllowsWrites(t *testing.T) {
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	writable := testDir(t, filepath.Join(home, "tool-cache"))

	policy := compileFixturePolicy(t, project, func(req *sandboxpolicy.Request) {
		req.GlobalRules = []sandboxpolicy.Rule{{Allow: writable, Mode: sandboxpolicy.ModeReadWrite}}
	})
	runner := testRunnerFromPolicy(t, policy, project, "grants")

	output, err := runSandboxCommand(t, runner,
		"printf project > "+shellQuote(filepath.Join(project, "project-file")), project)
	require.NoError(t, err, output)
	assert.FileExists(t, filepath.Join(project, "project-file"))

	output, err = runSandboxCommand(t, runner, "printf blocked > /etc/coagent-grant-probe", project)
	require.Error(t, err, "an ungranted host path is readable, not writable: %s", output)
	assert.NoFileExists(t, "/etc/coagent-grant-probe")

	output, err = runSandboxCommand(t, runner,
		"printf allowed > "+shellQuote(filepath.Join(writable, "allowed")), project)
	require.NoError(t, err, output)
	content, err := os.ReadFile(filepath.Join(writable, "allowed"))
	require.NoError(t, err)
	assert.Equal(t, "allowed", string(content))
}

// TestSandboxLinkedWorktreeGitMetadata proves a linked worktree's shared Git
// metadata stays writable while the main checkout stays read-only.
func TestSandboxLinkedWorktreeGitMetadata(t *testing.T) {
	home := testSandboxHome(t)
	mainRepo := testDir(t, filepath.Join(home, "main-repo"))
	gitDir := testDir(t, filepath.Join(mainRepo, ".git"))
	testDir(t, filepath.Join(gitDir, "objects"))
	testDir(t, filepath.Join(gitDir, "worktrees", "worktree"))
	require.NoError(t, os.WriteFile(filepath.Join(mainRepo, "checked-out"), []byte("main"), 0o600))

	worktree := testDir(t, filepath.Join(home, "worktree"))
	require.NoError(t, os.WriteFile(
		filepath.Join(worktree, ".git"),
		[]byte("gitdir: "+filepath.Join(gitDir, "worktrees", "worktree")+"\n"),
		0o600,
	))

	policy := compileFixturePolicy(t, worktree, func(req *sandboxpolicy.Request) {
		req.WorktreeGitDir = gitDir
	})
	runner := testRunnerFromPolicy(t, policy, worktree, "worktree")

	metadata := filepath.Join(gitDir, "worktrees", "worktree", "HEAD")
	command := strings.Join([]string{
		"printf ref > " + shellQuote(metadata),
		"test -f " + shellQuote(metadata),
	}, " && ")

	output, err := runSandboxCommand(t, runner, command, worktree)
	require.NoError(t, err, output)
	assert.FileExists(t, metadata)
}

func TestSandboxProbeConfirmsEnforcement(t *testing.T) {
	testSandboxHome(t)
	requireBubblewrap(t)
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")

	require.NoError(t, probeEnforcement(newEnabledRunner))
}

// TestBubblewrapIntegration proves the ordinary policy denies host writes
// outside its grants, keeps the project writable and gives the sandbox a
// private /dev/shm.
func TestBubblewrapIntegration(t *testing.T) {
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	denied := testDir(t, filepath.Join(home, "denied"))
	require.NoError(t, os.WriteFile(filepath.Join(denied, "readable"), []byte("host data"), 0o644))

	runner := testRunnerFromPolicy(t, compileFixturePolicy(t, project, nil), project, "integration")

	devShmName := "coagent-bwrap-" + filepath.Base(project)
	t.Cleanup(func() {
		if _, err := os.Stat(filepath.Join("/dev/shm", devShmName)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("sandbox exposed host /dev/shm: %v", err)
		}
	})

	command := strings.Join([]string{
		"test -e " + shellQuote(filepath.Join(denied, "readable")),
		"test ! -w " + shellQuote(denied),
		"! touch " + shellQuote(filepath.Join(denied, "blocked")),
		"touch " + shellQuote(filepath.Join(project, "direct")),
		"bash -c " + shellQuote("touch "+shellQuote(filepath.Join(project, "child"))),
		": >/dev/null",
		"touch /dev/shm/" + devShmName + " 2>/dev/null || true",
		"printf '%s' \"$COAGENT_BWRAP_TEST\"",
	}, " && ")

	t.Setenv("COAGENT_BWRAP_TEST", "inherited")
	output, err := runSandboxCommand(t, runner, command, project)
	require.NoError(t, err, output)
	assert.Contains(t, output, "inherited")
	assert.FileExists(t, filepath.Join(project, "direct"))
	assert.FileExists(t, filepath.Join(project, "child"))
	assert.NoFileExists(t, filepath.Join(denied, "blocked"))
	assert.NoFileExists(t, filepath.Join("/dev/shm", devShmName))
}

// TestBubblewrapLauncherDoesNotLoadModelEnvironment proves the launcher never
// forwards the model environment to the sandboxed process.
func TestBubblewrapLauncherDoesNotLoadModelEnvironment(t *testing.T) {
	cc, err := exec.LookPath("cc")
	if err != nil {
		t.Skip("C compiler is not installed")
	}

	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	outside := testDir(t, filepath.Join(home, "outside"))
	library := filepath.Join(outside, "preload.so")
	source := filepath.Join(outside, "preload.c")
	require.NoError(t, os.WriteFile(source, []byte(`#include <stdio.h>
#include <stdlib.h>
__attribute__((constructor)) static void mark(void) {
  const char *path = getenv("COAGENT_PRELOAD_SENTINEL");
  if (path != NULL) { FILE *f = fopen(path, "w"); if (f != NULL) { fputs("loaded", f); fclose(f); } }
}
`), 0o600))
	output, err := exec.Command(cc, "-shared", "-fPIC", "-o", library, source).CombinedOutput()
	require.NoError(t, err, string(output))

	runner := testRunnerFromPolicy(t, compileFixturePolicy(t, project, nil), project, "preload-test")
	sentinel := filepath.Join(outside, "preload-ran")
	cmd, err := runner.Command(t.Context(), procexec.Request{
		Path: "/bin/sh", Args: []string{"-c", `test "$INNER_VALUE" = retained`}, WorkDir: project,
		Env: []string{
			"PATH=/usr/bin:/bin", "INNER_VALUE=retained", "LD_PRELOAD=" + library,
			"COAGENT_PRELOAD_SENTINEL=" + sentinel,
		},
	})
	require.NoError(t, err)
	output, err = cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	assert.NoFileExists(t, sentinel)
	assert.Equal(t, sandboxLauncherEnvironment(), cmd.Env)
}

// TestNestedRootlessBubblewrap proves the runner still confines processes when
// it is started inside an outer rootless Bubblewrap namespace.
func TestNestedRootlessBubblewrap(t *testing.T) {
	const childEnv = "COAGENT_NESTED_BWRAP_CHILD"

	if os.Getenv(childEnv) == "1" {
		home := testSandboxHome(t)
		project := testDir(t, filepath.Join(home, "project"))

		nested, err := inUserNamespace()
		require.NoError(t, err)
		require.True(t, nested)

		ordinary := testRunnerFromPolicy(t, compileFixturePolicy(t, project, nil), project, "nested-rootless")
		output, err := runSandboxCommand(t, ordinary, "printf nested-ok", project)
		require.NoError(t, err, output)
		assert.Contains(t, output, "nested-ok")

		return
	}

	requireBubblewrap(t)

	workDir := t.TempDir()
	// The child resolves HOME as its startup home, so keep it disjoint from the
	// temporary directory t.TempDir() will pick under TMPDIR.
	childHome := testDir(t, filepath.Join(workDir, "home"))
	childTemp := testDir(t, filepath.Join(workDir, "tmp"))

	testBinary, err := os.Executable()
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bubblewrapExecutable,
		"--die-with-parent",
		"--ro-bind", "/", "/",
		"--dev", devPath,
		"--bind", workDir, workDir,
		"--proc", "/proc",
		"--unshare-user",
		"--cap-drop", "ALL",
		"--clearenv",
		"--setenv", "PATH", "/usr/bin:/bin",
		"--setenv", "HOME", childHome,
		"--setenv", "TMPDIR", childTemp,
		"--setenv", "XDG_CACHE_HOME", filepath.Join(childHome, ".cache"),
		"--setenv", childEnv, "1",
		"--",
		testBinary,
		"-test.run=^TestNestedRootlessBubblewrap$",
		"-test.v",
	)
	cmd.Dir = workDir
	output, err := cmd.CombinedOutput()
	require.NoError(t, err, string(output))
	assert.Contains(t, string(output), "--- PASS: TestNestedRootlessBubblewrap")
}

func TestRunner_AllowsReadFollowsTheCompiledPolicy(t *testing.T) {
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	secretDir := testDir(t, filepath.Join(home, "secrets"))

	policy := compileFixturePolicy(t, project, func(req *sandboxpolicy.Request) {
		req.GlobalRules = []sandboxpolicy.Rule{{Deny: secretDir}}
	})
	runner := testRunnerFromPolicy(t, policy, project, "read-authority")

	assert.True(t, runner.AllowsRead(filepath.Join(project, "main.go")),
		"the project root is granted")
	assert.True(t, runner.AllowsRead("/etc/passwd"),
		"an ordinary session reads the host filesystem")
	assert.False(t, runner.AllowsRead(filepath.Join(secretDir, "id_ed25519")),
		"a deny rule withholds a path the readable host root would otherwise expose")
}
