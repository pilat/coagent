//go:build linux

package bashsandbox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"github.com/pilat/coagent/internal/procexec"
	"github.com/pilat/coagent/internal/shellenv"
)

const (
	bubblewrapExecutable   = "bwrap"
	bubblewrapReadOnlyBind = "--ro-bind"
	mountInfoPath          = "/proc/self/mountinfo"
	maxUIDMapExtents       = 5
	maxUIDValue            = uint64(^uint32(0))
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

type uidMapExtent struct {
	inside  uint64
	outside uint64
	length  uint64
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
	procMountPoints, err := readProcMountPoints(mountInfoPath)
	if err != nil {
		return nil, fmt.Errorf("read Linux proc mounts: %w", err)
	}

	runner := &bubblewrapRunner{
		executable: executable,
		mounts:     buildMountOperations(policy.writableRoots, mountPoints),
		roots:      policy.writableRoots,
		policyKey:  policy.key(),
		policy:     policy,
	}
	if policy.readScope == ProjectConfined {
		if shieldedPolicyOverlapsProc(policy, procMountPoints) {
			return nil, errors.New("shielded policy overlaps a procfs mount")
		}

		runner.shieldMounts = buildShieldMountOperations(policy, mountPoints, procMountPoints)
	}
	if err := preflight(runner, policy.workDir); err != nil {
		return nil, fmt.Errorf("bubblewrap backend unusable: %w", err)
	}

	return runner, nil
}

func shieldedPolicyOverlapsProc(policy processPolicy, procMountPoints []string) bool {
	projectOverlap := pathOverlapsMount(policy.projectRoot, procMountPoints)

	workDirOverlap := pathOverlapsMount(policy.workDir, procMountPoints)
	if projectOverlap || workDirOverlap {
		return true
	}

	for _, mount := range policy.readMounts {
		sourceOverlap := pathOverlapsMount(mount.source, procMountPoints)

		targetOverlap := pathOverlapsMount(mount.target, procMountPoints)
		if sourceOverlap || targetOverlap {
			return true
		}
	}

	return false
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
		"--dev", devPath,
	}

	for _, mount := range r.mounts {
		operation := "--bind"
		if mount.readOnly {
			operation = bubblewrapReadOnlyBind
		}

		args = append(args, operation, mount.path, mount.path)
	}

	args = append(args, "--proc", "/proc")

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
		"--dev", devPath,
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

	args = append(args, "--proc", "/proc")

	return append(args, "--remount-ro", "/", "--chdir", workDir, "--")
}

func resolveBubblewrapExecutable(writableRoots []string) (string, error) {
	nested, err := inUserNamespace()
	if err != nil {
		return "", fmt.Errorf("detect Linux user namespace: %w", err)
	}

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

	if err := validateBubblewrapExecutable(executable, info.Mode(), stat.Uid, nested, writableRoots); err != nil {
		return "", err
	}

	return executable, nil
}

func validateBubblewrapExecutable(
	executable string,
	mode os.FileMode,
	uid uint32,
	nested bool,
	writableRoots []string,
) error {
	if !mode.IsRegular() || mode.Perm()&0o111 == 0 {
		return fmt.Errorf("bubblewrap executable %q is not an executable regular file", executable)
	}

	if !trustedBubblewrapOwner(executable, uid, nested, writableRoots) {
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

func trustedBubblewrapOwner(
	executable string,
	uid uint32,
	nested bool,
	writableRoots []string,
) bool {
	if !nested {
		return uid == 0
	}

	if uid == 0 {
		return false
	}

	return trustedUnmappedRootOwner(executable, uid, nested, writableRoots)
}

// Root-owned files appear as the kernel overflow UID inside a rootless outer
// user namespace. Trust that view only on a read-only mount outside writable roots.
func trustedUnmappedRootOwner(
	executable string,
	uid uint32,
	nested bool,
	writableRoots []string,
) bool {
	expectedUID, err := overflowUID()
	if err != nil {
		return false
	}

	var stat unix.Statfs_t
	if err := unix.Statfs(executable, &stat); err != nil {
		return false
	}

	return trustedUnmappedRootOwnerWith(
		executable,
		uid,
		expectedUID,
		nested,
		stat.Flags&unix.ST_RDONLY != 0,
		writableRoots,
	)
}

func trustedUnmappedRootOwnerWith(
	executable string,
	uid, overflowUID uint32,
	nested, readOnly bool,
	writableRoots []string,
) bool {
	if uid != overflowUID || overflowUID == 0 || !nested || !readOnly {
		return false
	}

	for _, root := range writableRoots {
		if pathWithinRoot(executable, root) {
			return false
		}
	}

	return true
}

func overflowUID() (uint32, error) {
	value, err := os.ReadFile("/proc/sys/kernel/overflowuid")
	if err != nil {
		return 0, fmt.Errorf("read kernel overflow UID: %w", err)
	}

	uid, err := strconv.ParseUint(strings.TrimSpace(string(value)), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse kernel overflow UID: %w", err)
	}

	return uint32(uid), nil
}

func inUserNamespace() (bool, error) {
	value, err := os.ReadFile("/proc/self/uid_map")
	if err != nil {
		return false, fmt.Errorf("read user namespace UID map: %w", err)
	}

	return parseUserNamespaceMap(string(value))
}

func parseUserNamespaceMap(value string) (bool, error) {
	lines := strings.Split(strings.TrimSpace(value), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return false, errors.New("user namespace UID map is empty")
	}

	if len(lines) > maxUIDMapExtents {
		return false, fmt.Errorf("user namespace UID map has %d extents", len(lines))
	}

	extents := make([]uidMapExtent, 0, len(lines))
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return false, fmt.Errorf("user namespace UID map line %q has %d fields", line, len(fields))
		}

		values := [3]uint64{}

		for index, field := range fields {
			parsed, err := strconv.ParseUint(field, 10, 64)
			if err != nil {
				return false, fmt.Errorf("parse user namespace UID map field %q: %w", field, err)
			}

			values[index] = parsed
		}

		extent := uidMapExtent{inside: values[0], outside: values[1], length: values[2]}
		if err := validateUIDMapExtent(extent); err != nil {
			return false, err
		}

		for _, existing := range extents {
			if uidMapRangesOverlap(existing.inside, existing.length, extent.inside, extent.length) ||
				uidMapRangesOverlap(existing.outside, existing.length, extent.outside, extent.length) {
				return false, errors.New("user namespace UID map has overlapping extents")
			}
		}

		extents = append(extents, extent)
	}

	if len(extents) != 1 {
		return true, nil
	}

	first := extents[0]

	return first.inside != 0 || first.outside != 0 || first.length != maxUIDValue, nil
}

func validateUIDMapExtent(extent uidMapExtent) error {
	if extent.length == 0 {
		return errors.New("user namespace UID map has a zero-length extent")
	}

	if extent.inside > maxUIDValue || extent.outside > maxUIDValue || extent.length > maxUIDValue {
		return errors.New("user namespace UID map extent exceeds UID range")
	}

	if extent.inside > maxUIDValue+1-extent.length || extent.outside > maxUIDValue+1-extent.length {
		return errors.New("user namespace UID map extent overflows UID range")
	}

	return nil
}

func uidMapRangesOverlap(leftStart, leftLength, rightStart, rightLength uint64) bool {
	return leftStart < rightStart+rightLength && rightStart < leftStart+leftLength
}
