//go:build linux

package bashsandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/shellenv"
)

const (
	bubblewrapExecutable   = "bwrap"
	bubblewrapReadOnlyBind = "--ro-bind"
	mountInfoPath          = "/proc/self/mountinfo"
)

var (
	_ Runner        = (*bubblewrapRunner)(nil)
	_ providerAware = (*bubblewrapRunner)(nil)
)

type bubblewrapRunner struct {
	executable   string
	mounts       []mountOperation
	shieldMounts []shieldMountOperation
	roots        []string
	policyKey    string
	provider     shellenv.Provider
	policy       processPolicy
}

// Command constructs a process confined by Bubblewrap.
//
//nolint:wsl_v5 // Request validation must precede command construction without shared state.
func (r *bubblewrapRunner) Command(
	ctx context.Context,
	request procexec.Request,
) (*exec.Cmd, error) {
	environment, err := innerEnvironment(request.Env, request.WorkDir)
	if err != nil {
		return nil, err
	}

	path := request.Path
	if r.policy.readScope == ProjectConfined {
		if err := r.policy.validateWorkDir(request.WorkDir); err != nil {
			return nil, err
		}
		var err error
		path, err = r.policy.resolveExecutable(path, request.WorkDir)
		if err != nil {
			return nil, err
		}
	}

	prefix := r.wrapPrefix(request.WorkDir)
	args := append([]string(nil), prefix[:len(prefix)-1]...)
	args = append(args, "--clearenv")
	for _, entry := range environment {
		args = append(args, "--setenv", entry.name, entry.value)
	}
	args = append(args, "--", path)
	args = append(args, request.Args...)
	cmd := exec.CommandContext(ctx, r.executable, args...)
	if r.policy.readScope == ProjectConfined {
		cmd.Dir = "/"
	} else {
		cmd.Dir = request.WorkDir
	}
	cmd.Env = sandboxLauncherEnvironment()

	return cmd, nil
}

func (r *bubblewrapRunner) BashCommand(
	ctx context.Context,
	command, workDir string,
	commandArgs ...string,
) (*exec.Cmd, error) {
	return r.Command(ctx, procexec.Request{
		Path:    bashExecutable,
		Args:    append([]string{"-c", command}, commandArgs...),
		WorkDir: workDir,
	})
}

// ShellCommand runs a user command confined by Bubblewrap, sourcing workDir's
// snapshot inside the sandbox when one is available. The snapshot file and
// $SHELL are visible via the existing `--ro-bind / /`, so no new bind mount.
func (r *bubblewrapRunner) ShellCommand(ctx context.Context, command, workDir string) (*exec.Cmd, error) {
	shell, snap := snapshotFor(ctx, r.provider, workDir)
	if snap == "" {
		return r.BashCommand(ctx, command, workDir)
	}

	return r.Command(ctx, procexec.Request{
		Path:    shell,
		Args:    []string{"-c", sourceLine(snap, command)},
		WorkDir: workDir,
	})
}

func (r *bubblewrapRunner) WritableRoots() []string {
	return append([]string(nil), r.roots...)
}

func (r *bubblewrapRunner) PolicyKey() string    { return r.policyKey }
func (r *bubblewrapRunner) ReadScope() ReadScope { return r.policy.readScope }

func (r *bubblewrapRunner) setProvider(p shellenv.Provider) { r.provider = p }

//nolint:wsl_v5 // Platform discovery and preflight are one runner construction boundary.
func newEnabledRunner(policy processPolicy) (Runner, error) {
	executable, err := resolveBubblewrapExecutable(policy.writableRoots)
	if err != nil {
		return nil, err
	}

	mountPoints, err := readMountPoints(mountInfoPath)
	if err != nil {
		return nil, fmt.Errorf("read Linux mount table: %w", err)
	}

	runner := &bubblewrapRunner{
		executable: executable,
		mounts:     buildMountOperations(policy.writableRoots, mountPoints),
		roots:      policy.writableRoots,
		policyKey:  policy.key(),
		policy:     policy,
	}
	if policy.readScope == ProjectConfined {
		runner.shieldMounts = buildShieldMountOperations(policy, mountPoints)
	}
	if err := preflight(runner, policy.workDir); err != nil {
		return nil, fmt.Errorf("bubblewrap backend unusable: %w", err)
	}

	return runner, nil
}

// wrapPrefix builds the bwrap flags up to and including the `--` separator; the
// caller appends the program and its arguments.
func (r *bubblewrapRunner) wrapPrefix(workDir string) []string {
	if r.policy.readScope == ProjectConfined {
		return r.shieldPrefix(workDir)
	}

	args := []string{
		"--die-with-parent",
		bubblewrapReadOnlyBind, "/", "/",
		"--dev", "/dev",
	}

	for _, mount := range r.mounts {
		operation := "--bind"
		if mount.readOnly {
			operation = bubblewrapReadOnlyBind
		}

		args = append(args, operation, mount.path, mount.path)
	}

	return append(args,
		"--unshare-user",
		"--cap-drop", "ALL",
		"--",
	)
}

//nolint:wsl_v5 // Namespace assembly follows the required mount order.
func (r *bubblewrapRunner) shieldPrefix(workDir string) []string {
	args := []string{
		"--die-with-parent",
		"--unshare-user",
		"--unshare-pid",
		"--cap-drop", "ALL",
		"--tmpfs", "/",
		"--dev", "/dev",
		"--proc", "/proc",
	}

	for _, dir := range shieldMountDirectories(r.shieldMounts) {
		args = append(args, "--dir", dir)
	}
	for _, mount := range r.shieldMounts {
		operation := "--bind"
		if mount.readOnly {
			operation = bubblewrapReadOnlyBind
		}
		args = append(args, operation, mount.source, mount.target)
	}

	return append(args, "--remount-ro", "/", "--chdir", workDir, "--")
}

func resolveBubblewrapExecutable(writableRoots []string) (string, error) {
	executable, err := exec.LookPath(bubblewrapExecutable)
	if err != nil {
		return "", fmt.Errorf("find Bubblewrap executable: %w", err)
	}

	executable, err = filepath.Abs(executable)
	if err != nil {
		return "", fmt.Errorf("make Bubblewrap executable path absolute: %w", err)
	}

	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("resolve Bubblewrap executable %q: %w", executable, err)
	}

	info, err := os.Stat(executable)
	if err != nil {
		return "", fmt.Errorf("stat Bubblewrap executable %q: %w", executable, err)
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", fmt.Errorf("inspect Bubblewrap executable %q ownership", executable)
	}

	if err := validateBubblewrapExecutable(executable, info.Mode(), stat.Uid, writableRoots); err != nil {
		return "", err
	}

	return executable, nil
}

func validateBubblewrapExecutable(
	executable string,
	mode os.FileMode,
	uid uint32,
	writableRoots []string,
) error {
	if !mode.IsRegular() || mode.Perm()&0o111 == 0 {
		return fmt.Errorf("bubblewrap executable %q is not an executable regular file", executable)
	}

	if uid != 0 {
		return fmt.Errorf("bubblewrap executable %q is not owned by root", executable)
	}

	if mode.Perm()&0o022 != 0 {
		return fmt.Errorf("bubblewrap executable %q is group- or world-writable", executable)
	}

	for _, root := range writableRoots {
		if pathWithinRoot(executable, root) {
			return fmt.Errorf("bubblewrap executable %q is under writable root %q", executable, root)
		}
	}

	return nil
}
