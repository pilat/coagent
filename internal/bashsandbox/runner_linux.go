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
	bubblewrapExecutable = "bwrap"
	mountInfoPath        = "/proc/self/mountinfo"
)

var (
	_ Runner        = (*bubblewrapRunner)(nil)
	_ providerAware = (*bubblewrapRunner)(nil)
)

type bubblewrapRunner struct {
	executable string
	mounts     []mountOperation
	roots      []string
	policyKey  string
	provider   shellenv.Provider
}

// Command constructs a process confined by Bubblewrap.
func (r *bubblewrapRunner) Command(
	ctx context.Context,
	request procexec.Request,
) (*exec.Cmd, error) {
	args := append(r.wrapPrefix(), request.Path)
	args = append(args, request.Args...)
	cmd := exec.CommandContext(ctx, r.executable, args...)
	cmd.Dir = request.WorkDir
	cmd.Env = request.Env

	return cmd, nil
}

func (r *bubblewrapRunner) BashCommand(ctx context.Context, command, workDir string, commandArgs ...string) (*exec.Cmd, error) {
	return r.Command(ctx, procexec.Request{
		Path:    "bash",
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

func (r *bubblewrapRunner) PolicyKey() string { return r.policyKey }

func (r *bubblewrapRunner) setProvider(p shellenv.Provider) { r.provider = p }

func newEnabledRunner(writableRoots []string, sessionKey ...string) (Runner, error) {
	executable, err := resolveBubblewrapExecutable(writableRoots)
	if err != nil {
		return nil, err
	}

	mountPoints, err := readMountPoints(mountInfoPath)
	if err != nil {
		return nil, fmt.Errorf("read Linux mount table: %w", err)
	}

	runner := &bubblewrapRunner{
		executable: executable,
		mounts:     buildMountOperations(writableRoots, mountPoints),
		roots:      writableRoots,
		policyKey:  policyKey(writableRoots, firstSessionKey(sessionKey)),
	}
	if err := preflight(runner); err != nil {
		return nil, fmt.Errorf("bubblewrap backend unusable: %w", err)
	}

	return runner, nil
}

// wrapPrefix builds the bwrap flags up to and including the `--` separator; the
// caller appends the program and its arguments.
func (r *bubblewrapRunner) wrapPrefix() []string {
	args := []string{
		"--die-with-parent",
		"--ro-bind", "/", "/",
		"--dev", "/dev",
	}

	for _, mount := range r.mounts {
		operation := "--bind"
		if mount.readOnly {
			operation = "--ro-bind"
		}

		args = append(args, operation, mount.path, mount.path)
	}

	return append(args,
		"--unshare-user",
		"--cap-drop", "ALL",
		"--",
	)
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
