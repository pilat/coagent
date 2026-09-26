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
	"github.com/pilat/coagent/internal/sandboxnet"
	"github.com/pilat/coagent/internal/sandboxpolicy"
	"github.com/pilat/coagent/internal/shellenv"
)

const (
	bubblewrapExecutable = "bwrap"
	bubblewrapReadOnlyFD = "--ro-bind-fd"
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
	network    *NetworkLink
}

type uidMapExtent struct {
	inside  uint64
	outside uint64
	length  uint64
}

// Command constructs a process confined by Bubblewrap.
func (r *bubblewrapRunner) Command(ctx context.Context, request procexec.Request) (*exec.Cmd, error) {
	return r.command(ctx, request, nil)
}

// GuestDialCommand exposes one handoff socket only to its own helper process.
func (r *bubblewrapRunner) GuestDialCommand(
	ctx context.Context, request procexec.Request, socket string,
) (*exec.Cmd, error) {
	return r.command(ctx, request, []sandboxpolicy.Grant{{
		Source: socket, Target: guestDialSocket, Mode: sandboxpolicy.ModeReadOnly,
		Kind: sandboxpolicy.KindFile, Present: true,
	}})
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
func (r *bubblewrapRunner) PolicyDigest() string { return r.policy.digest }

// AllowsRead reports the compiled policy's read authority, so a caller that
// hashes on-disk state reads only what the session may read.
func (r *bubblewrapRunner) AllowsRead(path string) bool { return r.policy.allowsRead(path) }

// SnapshotShell builds a command that sources the snapshot at its private
// runtime path, mounted read-only for that command only. Only the owning
// snapshot is exposed; the user cache directory never is.
func (r *bubblewrapRunner) SnapshotShell(
	ctx context.Context,
	shell, snapshot, command, workDir string,
	env []string,
) (*exec.Cmd, error) {
	line := command
	var extra []sandboxpolicy.Grant

	if snapshot != "" {
		line = sourceLine(snapshotRuntimePath(snapshot), command)
		extra = snapshotGrant(snapshot)
	}

	return r.command(ctx, procexec.Request{
		Path:    shell,
		Args:    []string{"-c", line},
		WorkDir: workDir,
		Env:     env,
	}, extra)
}

func (r *bubblewrapRunner) WritableRoots() []string {
	return append([]string(nil), r.roots...)
}

func (r *bubblewrapRunner) ProjectRoot() string { return r.policy.projectRoot }

func (r *bubblewrapRunner) PolicyKey() string    { return r.policyKey }
func (r *bubblewrapRunner) ReadScope() ReadScope { return r.policy.readScope }

func (r *bubblewrapRunner) setProvider(p shellenv.Provider) { r.provider = p }

func (r *bubblewrapRunner) command(
	ctx context.Context,
	request procexec.Request,
	extra []sandboxpolicy.Grant,
) (*exec.Cmd, error) {
	environment, err := innerEnvironment(request.Env, request.WorkDir)
	if err != nil {
		return nil, err
	}

	if err := r.policy.validateWorkDir(request.WorkDir); err != nil {
		return nil, err
	}

	path := request.Path
	if r.network == nil || request.Path != EntryBinaryPath {
		path, err = r.policy.resolveExecutable(request.Path, request.WorkDir)
		if err != nil {
			return nil, err
		}
	}

	plan, err := r.currentMountPlan()
	if err != nil {
		return nil, err
	}

	baseFiles, err := networkExtraFiles(r.network)
	if err != nil {
		return nil, err
	}

	plan, mountFiles, err := pinMountPlan(plan, 3+len(baseFiles))
	if err != nil {
		for _, file := range baseFiles {
			_ = file.Close()
		}

		return nil, err
	}

	extraPlan, extraFiles, err := pinMountPlan(buildExtraPlan(extra), 3+len(baseFiles)+len(mountFiles))
	if err != nil {
		for _, file := range baseFiles {
			_ = file.Close()
		}

		for _, file := range mountFiles {
			_ = file.Close()
		}

		return nil, err
	}

	cmd := exec.CommandContext(ctx, r.executable, r.argv(request, path, environment, plan, extraPlan)...)
	cmd.Dir = "/"
	cmd.Env = sandboxLauncherEnvironment()

	cmd.ExtraFiles = append(cmd.ExtraFiles, baseFiles...)
	cmd.ExtraFiles = append(cmd.ExtraFiles, mountFiles...)

	cmd.ExtraFiles = append(cmd.ExtraFiles, extraFiles...)
	if err := restrictLauncherCapabilities(cmd, r.policy.writableRoots); err != nil {
		procexec.CloseExtraFiles(cmd)
		return nil, err
	}

	return cmd, nil
}

// argv assembles the launcher invocation: Bubblewrap's flags, the cleared and
// rebuilt environment, then the join stage and the program itself.
func (r *bubblewrapRunner) argv(
	request procexec.Request,
	path string,
	environment []environmentEntry,
	plan, extraPlan mountPlan,
) []string {
	args := r.prefix(request.WorkDir, plan, extraPlan)
	args = append(args, "--clearenv")

	for _, entry := range environment {
		args = append(args, "--setenv", entry.name, entry.value)
	}

	args = append(args, "--")
	args = append(args, joinStage(r.network)...)
	args = append(args, path)

	return append(args, request.Args...)
}

// prefix builds the Bubblewrap flags before the command: a private root with a
// fresh proc, minimal dev and private shm, the policy's ordered binds, then the
// working directory. Raised shields are not a second builder — they are a
// policy that contains no profile entries. The caller appends `--clearenv`,
// the environment, the `--` separator and the program.
func (r *bubblewrapRunner) prefix(workDir string, plan, extraPlan mountPlan) []string {
	args := []string{
		"--die-with-parent",
	}

	if r.network == nil {
		args = append(args, "--unshare-user")
	}

	args = append(args,
		"--unshare-pid",
		"--unshare-ipc",
	)
	if r.network == nil {
		args = append(args, "--cap-drop", "ALL")
	} else {
		args = append(args, "--cap-add", "CAP_SYS_ADMIN", "--cap-add", "CAP_SETPCAP")
	}

	args = append(args, "--tmpfs", "/")

	dirs := plan.dirs
	if r.network != nil {
		dirs = parentDirectories(append([]bindMount{{target: EntryBinaryPath}}, plan.binds...))
	}

	for _, dir := range dirs {
		args = append(args, "--dir", dir)
	}

	for _, dir := range extraPlan.dirs {
		args = append(args, "--dir", dir)
	}

	args = append(args, "--proc", "/proc", "--dev", devPath, "--tmpfs", "/dev/shm")

	for _, mount := range plan.binds {
		args = append(args, bindOperation(mount), strconv.Itoa(mount.fd), mount.target)
	}

	for _, mount := range extraPlan.binds {
		args = append(args, bindOperation(mount), strconv.Itoa(mount.fd), mount.target)
	}

	args = append(args, networkPrefix(r.network)...)
	args = append(args, "--remount-ro", "/")

	return append(args, "--chdir", workDir)
}

// networkPrefix enters the owning user namespace and mounts the trusted entry
// binary plus per-generation resolver files. The network namespace FD remains
// inherited until the entry stage consumes and closes it.
func networkPrefix(link *NetworkLink) []string {
	if link == nil {
		return nil
	}

	return []string{
		"--userns", strconv.Itoa(userNSDescriptor),
		bubblewrapReadOnlyFD, "5", EntryBinaryPath,
		bubblewrapReadOnlyFD, "6", "/etc/resolv.conf",
		bubblewrapReadOnlyFD, "7", "/etc/hosts",
	}
}

// joinStage is the entry stage invocation that precedes the command: it joins
// the tree's network namespace before exec, then gives up the capability that
// made the join possible.
func joinStage(link *NetworkLink) []string {
	if link == nil {
		return nil
	}

	return []string{EntryBinaryPath, sandboxnet.JoinCommand, "--netns-fd", "4", "--"}
}

// networkExtraFiles are the descriptors a sandbox invocation inherits when it
// runs inside the tree's generation.
func networkExtraFiles(link *NetworkLink) ([]*os.File, error) {
	if link == nil || link.UserNS == nil {
		return nil, nil
	}

	owned := []*os.File{link.UserNS, link.NetNS, link.BinaryFile, link.Resolver, link.Hosts}

	files := make([]*os.File, 0, len(owned))
	for _, original := range owned {
		if original == nil {
			for _, file := range files {
				_ = file.Close()
			}

			return nil, errors.New("network generation is missing a required descriptor")
		}

		fd, err := unix.FcntlInt(original.Fd(), unix.F_DUPFD_CLOEXEC, 0)
		if err != nil {
			for _, file := range files {
				_ = file.Close()
			}

			return nil, fmt.Errorf("duplicate network descriptor: %w", err)
		}

		files = append(files, os.NewFile(uintptr(fd), original.Name()))
	}

	return files, nil
}

func bindOperation(mount bindMount) string {
	if mount.readOnly {
		return bubblewrapReadOnlyFD
	}

	return "--bind-fd"
}

func (r *bubblewrapRunner) currentMountPlan() (mountPlan, error) {
	procMountPoints, err := readProcMountPoints(mountInfoPath)
	if err != nil {
		return mountPlan{}, fmt.Errorf("read Linux proc mounts: %w", err)
	}

	if policyOverlapsProc(r.policy, procMountPoints) {
		return mountPlan{}, errors.New("sandbox policy overlaps a procfs mount")
	}

	grants, err := materializeGrants(r.policy)
	if err != nil {
		return mountPlan{}, err
	}

	sockets, err := materializeSockets(r.policy.sockets)
	if err != nil {
		return mountPlan{}, err
	}

	mountPoints, err := readMountPoints(mountInfoPath)
	if err != nil {
		return mountPlan{}, fmt.Errorf("read Linux mount table: %w", err)
	}

	return buildMountPlan(grants, sockets, mountPoints), nil
}

// newEnabledRunner builds a runner that keeps the daemon's network namespace;
// probes use it through the runnerFactory contract.
func newEnabledRunner(policy processPolicy) (Runner, error) {
	return newEnabledRunnerWithNetwork(policy, nil)
}

// newEnabledRunnerWithNetwork builds a runner that runs its commands inside the
// tree's network generation.
func newEnabledRunnerWithNetwork(policy processPolicy, network *NetworkLink) (Runner, error) {
	if network != nil &&
		(network.UserNS == nil || network.NetNS == nil || network.BinaryFile == nil || network.Resolver == nil || network.Hosts == nil) {
		return nil, errors.New("network generation is missing a required descriptor")
	}

	executable, err := resolveBubblewrapExecutable(policy.writableRoots)
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
		network:    network,
	}

	if err := preflight(runner, policy.workDir); err != nil {
		return nil, fmt.Errorf("bubblewrap backend unusable: %w", err)
	}

	return runner, nil
}

func policyOverlapsProc(policy processPolicy, procMountPoints []string) bool {
	if pathOverlapsMount(policy.projectRoot, procMountPoints) || pathOverlapsMount(policy.workDir, procMountPoints) {
		return true
	}

	for _, grant := range policy.grants {
		if pathOverlapsMount(grant.Source, procMountPoints) || pathOverlapsMount(grant.Target, procMountPoints) {
			return true
		}
	}

	return false
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
