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
	"github.com/pilat/coagent/internal/sandboxpolicy"
	"github.com/pilat/coagent/internal/shellenv"
)

const (
	bubblewrapExecutable = "bwrap"
	mountInfoPath        = "/proc/self/mountinfo"
	maxUIDMapExtents     = 5
	maxUIDValue          = uint64(^uint32(0))
)

var (
	_ Runner        = (*bubblewrapRunner)(nil)
	_ providerAware = (*bubblewrapRunner)(nil)
)

type bubblewrapRunner struct {
	executable string
	roots      []string
	policyKey  string
	provider   shellenv.Provider
	policy     processPolicy
}

type uidMapExtent struct {
	inside  uint64
	outside uint64
	length  uint64
}

// Command constructs a process confined by Bubblewrap.
func (r *bubblewrapRunner) Command(ctx context.Context, request procexec.Request) (*exec.Cmd, error) {
	return r.command(ctx, request, "")
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

// ShellCommand runs a user command under the compiled policy, sourcing workDir's
// snapshot.
func (r *bubblewrapRunner) ShellCommand(ctx context.Context, command, workDir string) (*exec.Cmd, error) {
	shell, snap := snapshotFor(ctx, r.provider, r, workDir)
	if snap == "" {
		return r.BashCommand(ctx, command, workDir)
	}

	return r.SnapshotShell(ctx, shell, snap, command, workDir, nil)
}

// PolicyDigest identifies the effective policy a snapshot belongs to.
func (r *bubblewrapRunner) PolicyDigest() string { return r.policy.policy.Digest }

// AllowsRead reports the compiled policy's read authority, so a caller that
// hashes on-disk state reads only what the session may read.
func (r *bubblewrapRunner) AllowsRead(path string) bool { return r.policy.allowsRead(path) }

// SnapshotShell builds a command that sources the snapshot through an
// inherited file descriptor: the host root is read-only, so there is nowhere
// left to bind-mount a private copy, and /proc is always mounted by the launcher.
func (r *bubblewrapRunner) SnapshotShell(
	ctx context.Context,
	shell, snapshot, command, workDir string,
	env []string,
) (*exec.Cmd, error) {
	return r.command(ctx, procexec.Request{
		Path:    shell,
		Args:    []string{"-c", command},
		WorkDir: workDir,
		Env:     env,
	}, snapshot)
}

func (r *bubblewrapRunner) WritableRoots() []string {
	return append([]string(nil), r.roots...)
}

func (r *bubblewrapRunner) ProjectRoot() string { return r.policy.projectRoot }

func (r *bubblewrapRunner) PolicyKey() string    { return r.policyKey }
func (r *bubblewrapRunner) ReadScope() ReadScope { return r.policy.readScope() }

func (r *bubblewrapRunner) setProvider(p shellenv.Provider) { r.provider = p }

// command constructs one Bubblewrap invocation. snapshot, when non-empty, is
// the host path of a shell-env snapshot to source through an inherited
// descriptor rather than a bind mount — see SnapshotShell.
func (r *bubblewrapRunner) command(
	ctx context.Context,
	request procexec.Request,
	snapshot string,
) (*exec.Cmd, error) {
	environment, err := innerEnvironment(request.Env, request.WorkDir)
	if err != nil {
		return nil, err
	}

	if err := r.policy.validateWorkDir(request.WorkDir); err != nil {
		return nil, err
	}

	path, err := r.policy.resolveExecutable(request.Path, request.WorkDir)
	if err != nil {
		return nil, err
	}

	plan, err := r.currentMountPlan()
	if err != nil {
		return nil, err
	}

	plan, mountFiles, err := pinMountPlan(plan)
	if err != nil {
		return nil, err
	}

	args := request.Args

	var snapshotFile *os.File

	if snapshot != "" {
		snapshotFile, err = os.Open(snapshot)
		if err != nil {
			for _, file := range mountFiles {
				_ = file.Close()
			}

			return nil, fmt.Errorf("open shell-env snapshot %q: %w", snapshot, err)
		}

		args = withSnapshotSource(args, 3+len(mountFiles))
	}

	cmd := exec.CommandContext(ctx, r.executable, r.argv(request.WorkDir, path, args, environment, plan)...)
	cmd.Dir = "/"
	cmd.Env = sandboxLauncherEnvironment()

	cmd.ExtraFiles = append(cmd.ExtraFiles, mountFiles...)
	if snapshotFile != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, snapshotFile)
	}

	if err := restrictLauncherCapabilities(cmd, r.policy.policy); err != nil {
		procexec.CloseExtraFiles(cmd)

		return nil, err
	}

	return cmd, nil
}

// withSnapshotSource prefixes a `<shell> -c <script>` argv with a source of
// the snapshot at its inherited descriptor, which /proc always exposes.
func withSnapshotSource(args []string, fd int) []string {
	out := append([]string(nil), args...)
	if len(out) < 2 {
		return out
	}

	out[1] = "source /proc/self/fd/" + strconv.Itoa(fd) + "; " + out[1]

	return out
}

// argv assembles the launcher invocation: Bubblewrap's flags, the cleared and
// rebuilt environment, then the program itself.
func (r *bubblewrapRunner) argv(
	workDir, path string,
	requestArgs []string,
	environment []environmentEntry,
	plan mountPlan,
) []string {
	args := r.prefix(workDir, plan)
	args = append(args, "--clearenv")

	for _, entry := range environment {
		args = append(args, "--setenv", entry.name, entry.value)
	}

	args = append(args, "--", path)

	return append(args, requestArgs...)
}

// prefix builds the Bubblewrap flags before the command: a private user, pid and
// ipc namespace with no retained capability, then the plan's ordered filesystem
// operations.
func (r *bubblewrapRunner) prefix(workDir string, plan mountPlan) []string {
	args := []string{
		"--die-with-parent",
		"--unshare-user",
		"--unshare-pid",
		"--unshare-ipc",
		"--cap-drop", "ALL",
	}

	args = append(args, mountArgs(plan.ops)...)

	return append(args, "--chdir", workDir)
}

// mountArgs translates the ordered plan into launcher flags. The plan already
// carries the order; this function adds nothing of its own.
func mountArgs(ops []mountOp) []string {
	args := make([]string, 0, len(ops)*3)

	for _, op := range ops {
		switch op.kind {
		case opBindRO:
			args = append(args, "--ro-bind-fd", strconv.Itoa(op.fd), op.target)
		case opBindRW:
			args = append(args, "--bind-fd", strconv.Itoa(op.fd), op.target)
		case opEmptyFile:
			args = append(args, "--ro-bind-data", strconv.Itoa(op.fd), op.target)
		case opTmpfs:
			args = append(args, "--tmpfs", op.target)
		case opProc:
			args = append(args, "--proc", op.target)
		case opDev:
			args = append(args, "--dev", op.target)
		}
	}

	return args
}

func (r *bubblewrapRunner) currentMountPlan() (mountPlan, error) {
	procMountPoints, err := readProcMountPoints(mountInfoPath)
	if err != nil {
		return mountPlan{}, fmt.Errorf("read Linux proc mounts: %w", err)
	}

	if policyOverlapsProc(r.policy, procMountPoints) {
		return mountPlan{}, errors.New("sandbox policy overlaps a procfs mount")
	}

	mountPoints, err := readMountPoints(mountInfoPath)
	if err != nil {
		return mountPlan{}, fmt.Errorf("read Linux mount table: %w", err)
	}

	entries := make([]sandboxpolicy.Entry, 0, len(r.policy.policy.Entries))
	for _, entry := range r.policy.policy.EffectiveEntries() {
		current, err := entry.Refresh()
		if err != nil {
			return mountPlan{}, fmt.Errorf("refresh mount entry: %w", err)
		}

		entries = append(entries, current)
	}

	return buildMountPlan(entries, mountPoints)
}

// newEnabledRunner builds a runner for one compiled policy; probes use it
// through the runnerFactory contract.
func newEnabledRunner(policy processPolicy) (Runner, error) {
	executable, err := resolveBubblewrapExecutable(policy.policy)
	if err != nil {
		return nil, err
	}

	procMountPoints, err := readProcMountPoints(mountInfoPath)
	if err != nil {
		return nil, fmt.Errorf("read Linux proc mounts: %w", err)
	}

	if policyOverlapsProc(policy, procMountPoints) {
		return nil, errors.New("sandbox policy overlaps a procfs mount")
	}

	runner := &bubblewrapRunner{
		executable: executable,
		roots:      policy.writableRoots,
		policyKey:  policy.key(),
		policy:     policy,
	}

	if err := preflight(runner, policy.workDir); err != nil {
		return nil, fmt.Errorf("bubblewrap backend unusable: %w", err)
	}

	return runner, nil
}

func policyOverlapsProc(policy processPolicy, procMountPoints []string) bool {
	if pathOverlapsMount(policy.policy.ProjectRoot, procMountPoints) ||
		pathOverlapsMount(policy.workDir, procMountPoints) {
		return true
	}

	for _, entry := range policy.policy.EffectiveEntries() {
		if entry.Path == "/" {
			continue
		}

		if pathOverlapsMount(entry.Path, procMountPoints) {
			return true
		}
	}

	return false
}

func resolveBubblewrapExecutable(policy sandboxpolicy.Policy) (string, error) {
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

	if err := validateBubblewrapExecutable(
		executable,
		info.Mode(),
		stat.Uid,
		nested,
		writableLauncherPaths(policy, executable),
	); err != nil {
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
