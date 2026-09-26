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

type bindMount struct {
	source   string
	target   string
	readOnly bool
	fd       int
}

// mountPlan is the ordered set of directories and binds one sandbox needs.
type mountPlan struct {
	dirs  []string
	binds []bindMount
}

type mountInfoEntry struct {
	mountPoint string
	fsType     string
}

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

// buildMountPlan converts policy grants and socket grants into the ordered
// mounts a sandbox needs, including read-only mounts for host filesystems
// nested inside a granted directory.
func buildMountPlan(
	grants []sandboxpolicy.Grant,
	sockets []sandboxpolicy.SocketGrant,
	hostMountPoints []string,
) mountPlan {
	seen := make(map[string]bindMount, len(grants)+len(sockets))

	for _, grant := range grants {
		if !grant.Present {
			continue
		}

		addBind(seen, grant.Source, grant.Target, grant.Mode == sandboxpolicy.ModeReadOnly)
	}

	for _, grant := range grants {
		if !grant.Present {
			continue
		}

		addNestedMounts(seen, grant, hostMountPoints)
	}

	for _, socket := range sockets {
		if !socket.Present {
			continue
		}

		addBind(seen, socket.Path, socket.Path, false)
	}

	binds := orderBinds(seen)

	return mountPlan{dirs: parentDirectories(binds), binds: binds}
}

// buildExtraPlan mounts the additions one command needs — currently the owning
// shell-env snapshot — without touching the policy's own grant table.
func buildExtraPlan(grants []sandboxpolicy.Grant) mountPlan {
	if len(grants) == 0 {
		return mountPlan{}
	}

	seen := make(map[string]bindMount, len(grants))

	for _, grant := range grants {
		if !grant.Present {
			continue
		}

		addBind(seen, grant.Source, grant.Target, grant.Mode == sandboxpolicy.ModeReadOnly)
	}

	binds := orderBinds(seen)

	return mountPlan{dirs: parentDirectories(binds), binds: binds}
}

func addBind(seen map[string]bindMount, source, target string, readOnly bool) {
	existing, ok := seen[target]
	if !ok {
		seen[target] = bindMount{source: source, target: target, readOnly: readOnly}

		return
	}

	if existing.readOnly && !readOnly {
		seen[target] = bindMount{source: source, target: target}
	}
}

// addNestedMounts keeps a host mount inside a granted directory read-only
// instead of letting the recursive bind pull it in with the ancestor's mode.
func addNestedMounts(seen map[string]bindMount, grant sandboxpolicy.Grant, hostMountPoints []string) {
	for _, mountPoint := range hostMountPoints {
		if mountPoint == grant.Source || !pathWithinRoot(mountPoint, grant.Source) {
			continue
		}

		relative, err := filepath.Rel(grant.Source, mountPoint)
		if err != nil {
			continue
		}

		addBind(seen, mountPoint, filepath.Join(grant.Target, relative), true)
	}
}

func orderBinds(seen map[string]bindMount) []bindMount {
	ordered := make([]bindMount, 0, len(seen))
	for _, mount := range seen {
		ordered = append(ordered, mount)
	}

	sort.Slice(ordered, func(i, j int) bool {
		left, right := pathDepth(ordered[i].target), pathDepth(ordered[j].target)
		if left != right {
			return left < right
		}

		return ordered[i].target < ordered[j].target
	})

	return ordered
}

// parentDirectories lists the directories a sandbox creates inside its private
// root before binding the granted targets.
func parentDirectories(binds []bindMount) []string {
	seen := make(map[string]struct{})

	for _, mount := range binds {
		for dir := filepath.Dir(mount.target); dir != "/" && dir != "."; dir = filepath.Dir(dir) {
			seen[dir] = struct{}{}
		}
	}

	dirs := make([]string, 0, len(seen))
	for dir := range seen {
		dirs = append(dirs, dir)
	}

	sort.Slice(dirs, func(i, j int) bool {
		left, right := pathDepth(dirs[i]), pathDepth(dirs[j])
		if left != right {
			return left < right
		}

		return dirs[i] < dirs[j]
	})

	return dirs
}

// materializeGrants creates the read-write directories a profile declared but
// which do not exist yet, and returns the grants with those objects marked
// present so the mount plan includes them. Read-only declarations are simply
// omitted.
func materializeGrants(policy processPolicy) ([]sandboxpolicy.Grant, error) {
	grants := make([]sandboxpolicy.Grant, 0, len(policy.grants))

	for _, grant := range policy.grants {
		info, err := os.Lstat(grant.Source)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 ||
				(grant.Kind == sandboxpolicy.KindDir && !info.IsDir()) ||
				(grant.Kind == sandboxpolicy.KindFile && !info.Mode().IsRegular()) {
				return nil, fmt.Errorf("grant source %q changed kind", grant.Source)
			}

			grant.Present = true
			grants = append(grants, grant)

			continue
		}

		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("inspect grant source %q: %w", grant.Source, err)
		}

		grant.Present = false
		if grant.Mode != sandboxpolicy.ModeReadWrite || grant.Kind != sandboxpolicy.KindDir {
			grants = append(grants, grant)
			continue
		}

		if err := createGrantedDir(grant.Source); err != nil {
			return nil, err
		}

		grant.Present = true
		grants = append(grants, grant)
	}

	return grants, nil
}

func materializeSockets(sockets []sandboxpolicy.SocketGrant) ([]sandboxpolicy.SocketGrant, error) {
	updated := make([]sandboxpolicy.SocketGrant, 0, len(sockets))
	for _, socket := range sockets {
		info, err := os.Lstat(socket.Path)
		if os.IsNotExist(err) {
			socket.Present = false
		} else if err != nil {
			return nil, fmt.Errorf("inspect granted socket %q: %w", socket.Path, err)
		} else if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("granted socket %q changed kind", socket.Path)
		} else {
			socket.Present = true
		}

		updated = append(updated, socket)
	}

	return updated, nil
}

// pinMountPlan opens every source before launching Bubblewrap. The descriptors
// are inherited by the launcher, so a rename after validation cannot redirect
// a bind to another object.
func pinMountPlan(plan mountPlan, firstFD int) (mountPlan, []*os.File, error) {
	pinned := mountPlan{dirs: plan.dirs, binds: make([]bindMount, 0, len(plan.binds))}

	files := make([]*os.File, 0, len(plan.binds))
	for _, mount := range plan.binds {
		fd, err := unix.Openat2(unix.AT_FDCWD, mount.source, &unix.OpenHow{
			Flags: unix.O_PATH | unix.O_CLOEXEC, Resolve: unix.RESOLVE_NO_SYMLINKS,
		})
		if err != nil {
			for _, file := range files {
				_ = file.Close()
			}

			return mountPlan{}, nil, fmt.Errorf("pin mount source %q: %w", mount.source, err)
		}

		file := os.NewFile(uintptr(fd), mount.source)
		mount.fd = firstFD + len(files)
		files = append(files, file)
		pinned.binds = append(pinned.binds, mount)
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
