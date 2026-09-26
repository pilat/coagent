//go:build linux

package bashsandbox

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/sandboxpolicy"
)

// compileFixturePolicy compiles the shipped catalog plus test profiles for one
// project and applies the extra request fields a fixture needs.
func compileFixturePolicy(
	t *testing.T,
	project string,
	shields bool,
	overrides map[string]sandboxpolicy.Profile,
	mutate func(*sandboxpolicy.Request),
) sandboxpolicy.Policy {
	t.Helper()

	req := testPolicyRequestShields(t, project, shields)
	if mutate != nil {
		mutate(&req)
	}

	compiled, err := sandboxpolicy.Compile(testCatalog(t, overrides), req)
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

// requireUnixSocketTool skips a fixture that needs a client for a pathname
// socket inside the sandbox. socat is used rather than nc because distro nc is
// an /etc/alternatives symlink the reviewed substrate does not expose.
func requireUnixSocketTool(t *testing.T) {
	t.Helper()

	if _, err := exec.LookPath("socat"); err != nil {
		t.Skip("socat is not installed")
	}
}

// connectUnixSocket is the in-sandbox client for one pathname socket.
func connectUnixSocket(path string) string {
	return "socat -u /dev/null UNIX-CONNECT:" + shellQuote(path)
}

// startUnixListener serves a pathname socket for the lifetime of the test.
func startUnixListener(t *testing.T, path string) {
	t.Helper()

	listener, err := net.Listen("unix", path)
	require.NoError(t, err)

	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}

			_ = conn.Close()
		}
	}()
}

// TestSandboxDeniesUnrelatedHomeFile proves an arbitrary file outside the
// granted roots is not readable, even though it lives under the same HOME.
func TestSandboxDeniesUnrelatedHomeFile(t *testing.T) {
	home := testSandboxHome(t)
	secret := filepath.Join(home, "unrelated-secret")
	require.NoError(t, os.WriteFile(secret, []byte("top-secret"), 0o600))

	project := testDir(t, filepath.Join(home, "project"))
	runner := testRunnerFromPolicy(t, compileFixturePolicy(t, project, false, nil, nil), project, "home-deny")

	output, err := runSandboxCommand(t, runner, "cat "+shellQuote(secret), project)
	require.Error(t, err, "an unrelated home file must be unreadable: %s", output)
	assert.NotContains(t, output, "top-secret")

	content, err := os.ReadFile(secret)
	require.NoError(t, err)
	assert.Equal(t, "top-secret", string(content))
}

// TestSandboxDeniesOtherProjectDirectory proves another project's tree is
// neither readable nor writable.
func TestSandboxDeniesOtherProjectDirectory(t *testing.T) {
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	other := testDir(t, filepath.Join(home, "other-project"))
	secret := filepath.Join(other, "secret")
	require.NoError(t, os.WriteFile(secret, []byte("other-secret"), 0o600))
	blocked := filepath.Join(other, "blocked")

	runner := testRunnerFromPolicy(t, compileFixturePolicy(t, project, false, nil, nil), project, "other-project")

	output, err := runSandboxCommand(t, runner, "cat "+shellQuote(secret), project)
	require.Error(t, err, "another project's file must be unreadable: %s", output)
	assert.NotContains(t, output, "other-secret")

	output, err = runSandboxCommand(t, runner, "printf blocked > "+shellQuote(blocked), project)
	require.Error(t, err, "another project's tree must not be writable: %s", output)
	assert.NoFileExists(t, blocked)
}

// TestSandboxIsolatesHostTemp proves the host /tmp is replaced by the
// project's private temporary storage: host entries are invisible and sandbox
// writes never reach the host directory.
func TestSandboxIsolatesHostTemp(t *testing.T) {
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))

	hostTemp := os.TempDir()
	marker := filepath.Join(hostTemp, "coagent-host-tmp-"+strconv.FormatInt(time.Now().UnixNano(), 36))
	require.NoError(t, os.WriteFile(marker, []byte("host"), 0o600))
	t.Cleanup(func() { _ = os.Remove(marker) })

	// Force the temporary-path rewrite to be observable inside the sandbox.
	t.Setenv("TMPDIR", filepath.Join(home, "host-tmp"))

	runner := testRunnerFromPolicy(t, compileFixturePolicy(t, project, false, nil, nil), project, "host-temp")

	command := strings.Join([]string{
		"test ! -e " + shellQuote(marker),
		"test \"$TMPDIR\" = " + shellQuote(sandboxpolicy.TempPath),
		"printf private > /tmp/coagent-sandbox-write",
		"test -f /tmp/coagent-sandbox-write",
	}, " && ")

	output, err := runSandboxCommand(t, runner, command, project)
	require.NoError(t, err, output)
	assert.NoFileExists(t, filepath.Join(hostTemp, "coagent-sandbox-write"),
		"the sandbox /tmp must never be the host /tmp")
}

// TestSandboxSocketGrantsExposeOnlyGrantedSockets proves a pathname socket
// inside an explicitly mounted directory is reachable (the accepted directory
// grant behavior) while an ungranted socket path is not.
func TestSandboxSocketGrantsExposeOnlyGrantedSockets(t *testing.T) {
	requireUnixSocketTool(t)

	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	grantedDir := testDir(t, filepath.Join(home, "granted-sockets"))

	grantedSocket := filepath.Join(grantedDir, "agent.sock")
	ungrantedSocket := filepath.Join(home, "loose.sock")
	startUnixListener(t, grantedSocket)
	startUnixListener(t, ungrantedSocket)

	overrides := map[string]sandboxpolicy.Profile{
		"socket-fixture": {Mounts: []sandboxpolicy.Mount{{
			Path: grantedDir, Mode: sandboxpolicy.ModeReadOnly,
			Type: sandboxpolicy.LevelBasic, Kind: sandboxpolicy.KindDir,
		}}},
	}
	runner := testRunnerFromPolicy(t, compileFixturePolicy(t, project, false, overrides, nil), project, "sockets")

	output, err := runSandboxCommand(t, runner,
		"test -S "+shellQuote(grantedSocket)+" && "+connectUnixSocket(grantedSocket), project)
	require.NoError(t, err, "a socket inside a granted directory must be reachable: %s", output)

	output, err = runSandboxCommand(t, runner,
		"test ! -e "+shellQuote(ungrantedSocket)+" && "+connectUnixSocket(ungrantedSocket), project)
	require.Error(t, err, "an ungranted socket path must be unreachable: %s", output)
}

func TestSandboxExactSocketGrantPreservesItsPath(t *testing.T) {
	requireUnixSocketTool(t)
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	socket := filepath.Join(home, "agent.sock")
	startUnixListener(t, socket)
	overrides := map[string]sandboxpolicy.Profile{
		"agent": {Sockets: []sandboxpolicy.Socket{{Path: socket, Type: sandboxpolicy.LevelBasic}}},
	}
	runner := testRunnerFromPolicy(t, compileFixturePolicy(t, project, false, overrides, nil), project, "exact-socket")
	output, err := runSandboxCommand(t, runner, connectUnixSocket(socket), project)
	require.NoError(t, err, output)
}

func TestSandboxCommandsDoNotInheritMountSourceDescriptors(t *testing.T) {
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	runner := testRunnerFromPolicy(t, compileFixturePolicy(t, project, false, nil, nil), project, "fd-cleanup")
	output, err := runSandboxCommand(t, runner,
		"for fd in {3..32}; do if test -e /proc/self/fd/$fd; then echo inherited:$fd; exit 1; fi; done", project)
	require.NoError(t, err, output)
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
	escapeDir := filepath.Join(project, "escape-dir")
	require.NoError(t, os.Symlink(secret, escapeFile))
	require.NoError(t, os.Symlink(other, escapeDir))

	runner := testRunnerFromPolicy(t, compileFixturePolicy(t, project, false, nil, nil), project, "symlink")

	output, err := runSandboxCommand(t, runner, "cat "+shellQuote(escapeFile), project)
	require.Error(t, err, "a symlink out of the project must not be followed: %s", output)
	assert.NotContains(t, output, "escaped-secret")

	output, err = runSandboxCommand(t, runner, "test -e "+shellQuote(escapeDir), project)
	require.Error(t, err, "a symlinked directory outside the project must not be reachable: %s", output)

	output, err = runSandboxCommand(t, runner, "cat "+shellQuote(filepath.Join(escapeDir, "secret")), project)
	require.Error(t, err, "a symlinked directory must not expose its target: %s", output)
	assert.NotContains(t, output, "escaped-secret")
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

	runner := testRunnerFromPolicy(t, compileFixturePolicy(t, project, false, nil, nil), project, "nested")
	output, err := runSandboxCommand(t, runner,
		"touch "+shellQuote(outerFile)+" && ! touch "+shellQuote(nestedFile), project)
	require.NoError(t, err, output)
	assert.FileExists(t, outerFile)
	assert.NoFileExists(t, nestedFile)

	require.NoError(t, os.WriteFile(filepath.Join(nested, "readable"), []byte("nested"), 0o600))
	shielded := testRunnerFromPolicy(t, compileFixturePolicy(t, project, true, nil, nil), project, "nested-shielded")
	output, err = runSandboxCommand(t, shielded,
		"grep -q nested "+shellQuote(filepath.Join(nested, "readable"))+
			" && ! touch "+shellQuote(filepath.Join(nested, "shielded-blocked")), project)
	require.NoError(t, err, output)
	assert.NoFileExists(t, filepath.Join(nested, "shielded-blocked"))
}

// TestSandboxAppliesDeclaredReadOnlyAndReadWriteGrants proves the project is
// writable and a profile's declared modes are honored: read-only denies writes
// while read-write allows them, and raised shields remove both.
func TestSandboxAppliesDeclaredReadOnlyAndReadWriteGrants(t *testing.T) {
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	readOnly := testDir(t, filepath.Join(home, "tool-read-only"))
	readWrite := testDir(t, filepath.Join(home, "tool-read-write"))
	require.NoError(t, os.WriteFile(filepath.Join(readOnly, "config"), []byte("ro-config"), 0o600))

	overrides := map[string]sandboxpolicy.Profile{
		"fixture": {Mounts: []sandboxpolicy.Mount{
			{
				Path: readOnly, Mode: sandboxpolicy.ModeReadOnly,
				Type: sandboxpolicy.LevelBasic, Kind: sandboxpolicy.KindDir,
			},
			{
				Path: readWrite, Mode: sandboxpolicy.ModeReadWrite,
				Type: sandboxpolicy.LevelBasic, Kind: sandboxpolicy.KindDir,
			},
		}},
	}

	runner := testRunnerFromPolicy(t, compileFixturePolicy(t, project, false, overrides, nil), project, "grants")

	output, err := runSandboxCommand(t, runner,
		"printf project > "+shellQuote(filepath.Join(project, "project-file")), project)
	require.NoError(t, err, output)
	assert.FileExists(t, filepath.Join(project, "project-file"))

	output, err = runSandboxCommand(t, runner, "cat "+shellQuote(filepath.Join(readOnly, "config")), project)
	require.NoError(t, err, output)
	assert.Equal(t, "ro-config", strings.TrimSpace(output))

	output, err = runSandboxCommand(t, runner,
		"printf blocked > "+shellQuote(filepath.Join(readOnly, "blocked")), project)
	require.Error(t, err, "a read-only grant must deny writes: %s", output)
	assert.NoFileExists(t, filepath.Join(readOnly, "blocked"))

	output, err = runSandboxCommand(t, runner,
		"printf allowed > "+shellQuote(filepath.Join(readWrite, "allowed")), project)
	require.NoError(t, err, output)
	content, err := os.ReadFile(filepath.Join(readWrite, "allowed"))
	require.NoError(t, err)
	assert.Equal(t, "allowed", string(content))

	shielded := testRunnerFromPolicy(
		t,
		compileFixturePolicy(t, project, true, overrides, nil),
		project,
		"grants-shielded",
	)
	output, err = runSandboxCommand(t, shielded,
		"test ! -e "+shellQuote(filepath.Join(readOnly, "config"))+
			" && test ! -e "+shellQuote(filepath.Join(readWrite, "allowed")), project)
	require.NoError(t, err, "shields must remove profile grants: %s", output)
}

// TestSandboxProjectsDoNotShareTemp proves the private /tmp is project-scoped:
// another project cannot see it, while a sibling session of the same project
// can.
func TestSandboxProjectsDoNotShareTemp(t *testing.T) {
	home := testSandboxHome(t)
	first := testDir(t, filepath.Join(home, "project-one"))
	second := testDir(t, filepath.Join(home, "project-two"))

	firstRunner := testRunnerFromPolicy(t, compileFixturePolicy(t, first, false, nil, nil), first, "first")
	output, err := runSandboxCommand(t, firstRunner,
		"printf private > /tmp/shared-marker && cat /tmp/shared-marker", first)
	require.NoError(t, err, output)

	secondRunner := testRunnerFromPolicy(t, compileFixturePolicy(t, second, false, nil, nil), second, "second")
	output, err = runSandboxCommand(t, secondRunner, "test ! -e /tmp/shared-marker", second)
	require.NoError(t, err, "another project must not see this project's temp files: %s", output)

	sibling := testRunnerFromPolicy(t, compileFixturePolicy(t, first, false, nil, nil), first, "first-sibling")
	output, err = runSandboxCommand(t, sibling, "cat /tmp/shared-marker", first)
	require.NoError(t, err, "a sibling session of the same project shares its temp storage: %s", output)

	firstTemp, err := coagenthome.SandboxTempDir(coagenthome.SandboxPathIdentity(first))
	require.NoError(t, err)
	secondTemp, err := coagenthome.SandboxTempDir(coagenthome.SandboxPathIdentity(second))
	require.NoError(t, err)
	assert.NotEqual(t, firstTemp, secondTemp)
}

// TestSandboxProjectUnderHostTemp proves a project located under the host
// temporary directory still works and does not shadow its own mount.
func TestSandboxProjectUnderHostTemp(t *testing.T) {
	testSandboxHome(t)

	// t.TempDir() lives under the host temporary directory.
	project := t.TempDir()
	require.True(t, pathWithinRoot(project, os.TempDir()))

	hostFile := filepath.Join(project, "host-created")
	require.NoError(t, os.WriteFile(hostFile, []byte("host"), 0o600))

	runner := testRunnerFromPolicy(t, compileFixturePolicy(t, project, false, nil, nil), project, "under-temp")

	sandboxFile := filepath.Join(project, "sandbox-created")
	command := strings.Join([]string{
		"test -f " + shellQuote(hostFile),
		"printf sandbox > " + shellQuote(sandboxFile),
		"test -f " + shellQuote(sandboxFile),
		"printf private > /tmp/probe",
		"test ! -f " + shellQuote(filepath.Join(project, "probe")),
	}, " && ")

	output, err := runSandboxCommand(t, runner, command, project)
	require.NoError(t, err, output)
	assert.FileExists(t, sandboxFile)
	assert.NoFileExists(
		t,
		filepath.Join(project, "probe"),
		"the private /tmp must not resolve to the project directory",
	)
}

// TestSandboxLinkedWorktreeGitMetadata proves a linked worktree's shared Git
// metadata stays writable in both shield states while the main checkout stays
// unreadable.
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

	for _, shields := range []bool{false, true} {
		t.Run(fmt.Sprintf("shields=%t", shields), func(t *testing.T) {
			policy := compileFixturePolicy(t, worktree, shields, nil, func(req *sandboxpolicy.Request) {
				req.GitMetadataRoot = gitDir
			})
			runner := testRunnerFromPolicy(t, policy, worktree, "worktree")

			metadata := filepath.Join(gitDir, "worktrees", "worktree", "HEAD")
			command := strings.Join([]string{
				"printf ref > " + shellQuote(metadata),
				"test -f " + shellQuote(metadata),
				"test ! -e " + shellQuote(filepath.Join(mainRepo, "checked-out")),
			}, " && ")

			output, err := runSandboxCommand(t, runner, command, worktree)
			require.NoError(t, err, output)
			assert.FileExists(t, metadata)
		})
	}
}

func TestSandboxProbeConfirmsEnforcement(t *testing.T) {
	testSandboxHome(t)
	requireBubblewrap(t)
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")

	require.NoError(t, probeEnforcement(newEnabledRunner))
}

func TestSandboxShieldProbeConfirmsEnforcement(t *testing.T) {
	testSandboxHome(t)
	requireBubblewrap(t)
	t.Setenv("PATH", "/usr/bin:/bin:/usr/sbin:/sbin")

	require.NoError(t, probeShieldEnforcement(newEnabledRunner))
}

// TestBubblewrapIntegration proves the ordinary policy denies host reads and
// writes outside its grants, keeps the project writable and gives the sandbox a
// private /dev/shm.
func TestBubblewrapIntegration(t *testing.T) {
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	denied := testDir(t, filepath.Join(home, "denied"))
	require.NoError(t, os.WriteFile(filepath.Join(denied, "readable"), []byte("host data"), 0o644))

	runner := testRunnerFromPolicy(t, compileFixturePolicy(t, project, false, nil, nil), project, "integration")

	devShmName := "coagent-bwrap-" + filepath.Base(project)
	t.Cleanup(func() {
		if _, err := os.Stat(filepath.Join("/dev/shm", devShmName)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("sandbox exposed host /dev/shm: %v", err)
		}
	})

	command := strings.Join([]string{
		"test ! -e " + shellQuote(filepath.Join(denied, "readable")),
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

// TestBubblewrapShieldedLauncherDoesNotLoadModelEnvironment proves the launcher
// never forwards the model environment to the sandboxed process.
func TestBubblewrapShieldedLauncherDoesNotLoadModelEnvironment(t *testing.T) {
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

	runner := testRunnerFromPolicy(t, compileFixturePolicy(t, project, true, nil, nil), project, "preload-test")
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

// TestBubblewrapShieldedIntegration proves the raised-shields policy keeps the
// project writable while hiding unrelated files and giving a private /tmp.
func TestBubblewrapShieldedIntegration(t *testing.T) {
	home := testSandboxHome(t)
	project := testDir(t, filepath.Join(home, "project"))
	outside := filepath.Join(home, "outside-secret")
	require.NoError(t, os.WriteFile(outside, []byte("secret"), 0o600))

	runner := testRunnerFromPolicy(t, compileFixturePolicy(t, project, true, nil, nil), project, "shielded-test")

	command := strings.Join([]string{
		"printf project > project-file",
		"grep -q project project-file",
		"test ! -e " + shellQuote(outside),
		"printf private > /tmp/private-probe",
		"test -f /tmp/private-probe",
	}, " && ")
	output, err := runSandboxCommand(t, runner, command, project)
	require.NoError(t, err, output)
	assert.FileExists(t, filepath.Join(project, "project-file"))
	assert.NoFileExists(t, filepath.Join("/tmp", "private-probe"))
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

		outerPIDNamespace, err := os.Readlink("/proc/self/ns/pid")
		require.NoError(t, err)

		ordinary := testRunnerFromPolicy(
			t,
			compileFixturePolicy(t, project, false, nil, nil),
			project,
			"nested-rootless",
		)
		output, err := runSandboxCommand(t, ordinary, "printf nested-ok", project)
		require.NoError(t, err, output)
		assert.Contains(t, output, "nested-ok")

		shielded := testRunnerFromPolicy(
			t,
			compileFixturePolicy(t, project, true, nil, nil),
			project,
			"nested-rootless-shielded",
		)
		output, err = runSandboxCommand(t, shielded, strings.Join([]string{
			"test \"$(readlink /proc/self/ns/pid)\" != " + shellQuote(outerPIDNamespace),
			"printf private > /tmp/probe && test -f /tmp/probe",
		}, " && "), project)
		require.NoError(t, err, output)

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
	runner := testRunnerFromPolicy(t, compileFixturePolicy(t, project, false, nil, nil), project, "read-authority")

	assert.True(t, runner.AllowsRead(filepath.Join(project, "main.go")),
		"the project root is granted")
	assert.False(t, runner.AllowsRead("/etc/shadow"),
		"a host file outside every profile is not")
}
