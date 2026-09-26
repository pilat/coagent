//go:build linux

package bashsandbox

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/pilat/coagent/internal/sandboxpolicy"
)

// Later filesystem operations mount over earlier ones, so order determines access.
type opKind uint8

const (
	opBindRO opKind = iota
	opBindRW
	opTmpfs
	opEmptyFile
	opProc
	opDev
)

// mountOp is one ordered operation. source is a host path pinned to fd before
// launch; tmpfs operations carry a target only.
type mountOp struct {
	kind   opKind
	source string
	target string
	fd     int
}

// mountPlan is the ordered operation list one sandbox invocation applies.
type mountPlan struct {
	ops []mountOp
}

type mountInfoEntry struct {
	mountPoint string
	fsType     string
}

// kernelOwnedRoots are the pseudo-filesystems Bubblewrap installs itself. A
// host copy must never be bound over them.
var kernelOwnedRoots = []string{"/proc", "/dev"}

func readMountInfo(path string) ([]mountInfoEntry, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open mountinfo %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	return parseMountInfoEntries(file)
}

func readMountPoints(path string) ([]string, error) {
	entries, err := readMountInfo(path)
	if err != nil {
		return nil, err
	}

	mountPoints := make([]string, 0, len(entries))
	for _, entry := range entries {
		mountPoints = append(mountPoints, entry.mountPoint)
	}

	return mountPoints, nil
}

func readProcMountPoints(path string) ([]string, error) {
	entries, err := readMountInfo(path)
	if err != nil {
		return nil, err
	}

	mountPoints := make([]string, 0)

	for _, entry := range entries {
		if entry.fsType == "proc" {
			mountPoints = append(mountPoints, entry.mountPoint)
		}
	}

	return mountPoints, nil
}

func pathOverlapsMount(path string, mountPoints []string) bool {
	for _, mountPoint := range mountPoints {
		if pathWithinRoot(path, mountPoint) || pathWithinRoot(mountPoint, path) {
			return true
		}
	}

	return false
}

func parseMountInfoEntries(reader io.Reader) ([]mountInfoEntry, error) {
	var entries []mountInfoEntry

	seen := make(map[string]int)

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		line := scanner.Text()

		fields := strings.Fields(line)
		if len(fields) < 6 {
			return nil, fmt.Errorf("malformed mountinfo line %q", line)
		}

		separator := -1

		for index := 5; index < len(fields); index++ {
			if fields[index] == "-" {
				separator = index

				break
			}
		}

		if separator < 0 || separator+1 >= len(fields) {
			return nil, fmt.Errorf("mountinfo line %q has no filesystem type", line)
		}

		mountPoint, err := decodeMountInfoPath(fields[4])
		if err != nil {
			return nil, fmt.Errorf("decode mount point %q: %w", fields[4], err)
		}

		mountPoint = filepath.Clean(mountPoint)
		if !filepath.IsAbs(mountPoint) {
			return nil, fmt.Errorf("mount point %q is not absolute", mountPoint)
		}

		fsType := fields[separator+1]
		if index, ok := seen[mountPoint]; ok {
			if fsType == "proc" {
				entries[index].fsType = fsType
			}

			continue
		}

		seen[mountPoint] = len(entries)
		entries = append(entries, mountInfoEntry{mountPoint: mountPoint, fsType: fsType})
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan mountinfo: %w", err)
	}

	return entries, nil
}

func decodeMountInfoPath(value string) (string, error) {
	var decoded strings.Builder

	for i := 0; i < len(value); i++ {
		if value[i] != '\\' {
			decoded.WriteByte(value[i])

			continue
		}

		if i+3 >= len(value) {
			return "", errors.New("truncated escape")
		}

		octal := value[i+1 : i+4]

		decodedByte, err := strconv.ParseUint(octal, 8, 8)
		if err != nil {
			return "", fmt.Errorf("invalid escape \\%s", octal)
		}

		decoded.WriteByte(byte(decodedByte))

		i += 3
	}

	return decoded.String(), nil
}

// Nested host mounts need their own read-only binds before ordered policy entries.
// Fresh proc and dev mounts come last to prevent policy entries from shadowing them.
func buildMountPlan(entries []sandboxpolicy.Entry, hostMountPoints []string) (mountPlan, error) {
	entries = (sandboxpolicy.Policy{Entries: entries}).EffectiveEntries()
	if err := (sandboxpolicy.Policy{Entries: entries}).ValidateMounts(); err != nil {
		return mountPlan{}, fmt.Errorf("validate mount policy: %w", err)
	}

	plan := mountPlan{ops: []mountOp{{kind: opBindRO, source: "/", target: "/"}}}
	plan.ops = append(plan.ops, hostMountOps(hostMountPoints)...)

	for i, entry := range entries {
		if i == 0 && isHostRootEntry(entry) {
			continue
		}

		ops, err := entryOps(entry)
		if err != nil {
			return mountPlan{}, err
		}

		plan.ops = append(plan.ops, ops...)
		if isHostRootEntry(entry) {
			plan.ops = append(plan.ops, hostMountOps(hostMountPoints)...)
		}
	}

	// A fresh proc and dev come last so no host copy bound above can shadow them.
	plan.ops = append(plan.ops,
		mountOp{kind: opProc, target: "/proc"},
		mountOp{kind: opDev, target: devPath},
		mountOp{kind: opTmpfs, target: "/dev/shm"},
	)

	return plan, nil
}

func isHostRootEntry(entry sandboxpolicy.Entry) bool {
	return entry.Path == "/" && entry.Action == sandboxpolicy.ActionAllow && entry.Mode == sandboxpolicy.ModeReadOnly
}

// entryOps translates one policy entry into the operations it needs. An
// absent object is skipped, except an absent allow+rw directory, which is
// created on the host first so it can be bound.
func entryOps(entry sandboxpolicy.Entry) ([]mountOp, error) {
	if entry.Action == sandboxpolicy.ActionAllow && entry.Mode == sandboxpolicy.ModeReadWrite {
		return rwEntryOps(entry)
	}

	if entry.Action == sandboxpolicy.ActionAllow {
		if !entry.Present {
			return nil, nil
		}

		return []mountOp{{kind: opBindRO, source: entry.Path, target: entry.Path}}, nil
	}

	if !entry.Present {
		return nil, nil
	}

	if entry.Kind == sandboxpolicy.KindDir {
		return []mountOp{{kind: opTmpfs, target: entry.Path}}, nil
	}

	return []mountOp{{kind: opEmptyFile, target: entry.Path}}, nil
}

func rwEntryOps(entry sandboxpolicy.Entry) ([]mountOp, error) {
	if !entry.Present && entry.Kind == sandboxpolicy.KindDir {
		if err := createGrantedDir(entry.Path); err != nil {
			return nil, err
		}
	} else if !entry.Present {
		return nil, nil
	}

	return []mountOp{{kind: opBindRW, source: entry.Path, target: entry.Path}}, nil
}

// hostMountOps re-binds every host mount point read-only. Bubblewrap's
// read-only bind of the root applies MS_RDONLY to the top mount alone, so a
// nested mount would otherwise stay writable.
func hostMountOps(mountPoints []string) []mountOp {
	ordered := append([]string(nil), mountPoints...)
	sort.Slice(ordered, func(i, j int) bool {
		left, right := pathDepth(ordered[i]), pathDepth(ordered[j])
		if left != right {
			return left < right
		}

		return ordered[i] < ordered[j]
	})

	ops := make([]mountOp, 0, len(ordered))

	for _, mountPoint := range ordered {
		if mountPoint == "/" || kernelOwned(mountPoint) {
			continue
		}

		ops = append(ops, mountOp{kind: opBindRO, source: mountPoint, target: mountPoint})
	}

	return ops
}

func kernelOwned(path string) bool {
	for _, root := range kernelOwnedRoots {
		if pathWithinRoot(path, root) {
			return true
		}
	}

	return false
}

// pinMountPlan opens every bind source before launching Bubblewrap. The
// descriptors are inherited by the launcher, so a rename after validation
// cannot redirect a bind to another object. An empty-file mask shares one
// /dev/null descriptor.
func pinMountPlan(plan mountPlan) (mountPlan, []*os.File, error) {
	const firstFD = 3

	pinned := mountPlan{ops: make([]mountOp, 0, len(plan.ops))}
	files := make([]*os.File, 0, len(plan.ops))

	fail := func(err error) (mountPlan, []*os.File, error) {
		for _, file := range files {
			_ = file.Close()
		}

		return mountPlan{}, nil, err
	}

	emptyFD := -1

	for _, op := range plan.ops {
		switch op.kind {
		case opTmpfs, opProc, opDev:
			pinned.ops = append(pinned.ops, op)

			continue
		case opEmptyFile:
			if emptyFD < 0 {
				file, err := os.Open(os.DevNull)
				if err != nil {
					return fail(fmt.Errorf("open %s for mask: %w", os.DevNull, err))
				}

				emptyFD = firstFD + len(files)
				files = append(files, file)
			}

			op.fd = emptyFD
			pinned.ops = append(pinned.ops, op)

			continue
		case opBindRO, opBindRW:
		}

		fd, err := unix.Openat2(unix.AT_FDCWD, op.source, &unix.OpenHow{
			Flags: unix.O_PATH | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS,
		})
		if err != nil {
			return fail(fmt.Errorf("pin mount source %q: %w", op.source, err))
		}

		op.fd = firstFD + len(files)
		files = append(files, os.NewFile(uintptr(fd), op.source))
		pinned.ops = append(pinned.ops, op)
	}

	return pinned, files, nil
}

// createGrantedDir creates one declared directory through a rooted traversal
// that refuses to follow a symlink, so an approved grant cannot be redirected
// onto another object.
func createGrantedDir(path string) error {
	clean := filepath.Clean(path)

	parent, err := unix.Open("/", unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open filesystem root: %w", err)
	}

	defer func() { _ = unix.Close(parent) }()

	for part := range strings.SplitSeq(strings.TrimPrefix(clean, "/"), "/") {
		if part == "" {
			continue
		}

		next, openErr := unix.Openat2(parent, part, &unix.OpenHow{
			Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
			Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
		})
		if errors.Is(openErr, unix.ENOENT) {
			if mkdirErr := unix.Mkdirat(parent, part, 0o700); mkdirErr != nil && !errors.Is(mkdirErr, unix.EEXIST) {
				return fmt.Errorf("create granted directory %q: %w", path, mkdirErr)
			}

			next, openErr = unix.Openat2(parent, part, &unix.OpenHow{
				Flags:   unix.O_PATH | unix.O_DIRECTORY | unix.O_CLOEXEC,
				Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
			})
		}

		if openErr != nil {
			if errors.Is(openErr, unix.ELOOP) {
				return fmt.Errorf("granted path %q traverses symlink: %w", path, openErr)
			}

			if errors.Is(openErr, unix.ENOTDIR) {
				return fmt.Errorf("granted path %q has non-directory component: %w", path, openErr)
			}

			return fmt.Errorf("traverse granted directory %q: %w", path, openErr)
		}

		_ = unix.Close(parent)
		parent = next
	}

	return nil
}
