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
)

type mountOperation struct {
	path     string
	readOnly bool
}

type shieldMountOperation struct {
	source   string
	target   string
	readOnly bool
}

func readMountPoints(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open mountinfo %q: %w", path, err)
	}
	defer func() { _ = file.Close() }()

	return parseMountInfo(file)
}

func parseMountInfo(reader io.Reader) ([]string, error) {
	var mountPoints []string
	seen := make(map[string]struct{})

	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 {
			return nil, fmt.Errorf("malformed mountinfo line %q", scanner.Text())
		}

		mountPoint, err := decodeMountInfoPath(fields[4])
		if err != nil {
			return nil, fmt.Errorf("decode mount point %q: %w", fields[4], err)
		}

		mountPoint = filepath.Clean(mountPoint)
		if !filepath.IsAbs(mountPoint) {
			return nil, fmt.Errorf("mount point %q is not absolute", mountPoint)
		}

		if _, ok := seen[mountPoint]; ok {
			continue
		}

		seen[mountPoint] = struct{}{}
		mountPoints = append(mountPoints, mountPoint)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan mountinfo: %w", err)
	}

	return mountPoints, nil
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

func buildMountOperations(writableRoots, mountPoints []string) []mountOperation {
	operations := make(map[string]mountOperation, len(writableRoots)+len(mountPoints))
	for _, root := range writableRoots {
		operations[root] = mountOperation{path: root}
	}

	for _, mountPoint := range mountPoints {
		if _, explicitlyWritable := operations[mountPoint]; explicitlyWritable {
			continue
		}

		for _, root := range writableRoots {
			if mountPoint != root && pathWithinRoot(mountPoint, root) {
				// A mount point inside an explicit root must stay read-only:
				// special mounts (tmpfs, procfs) inside a writable root would
				// otherwise become a writable escape hatch.
				operations[mountPoint] = mountOperation{path: mountPoint, readOnly: true}

				break
			}
		}
	}

	ordered := make([]mountOperation, 0, len(operations))
	for _, operation := range operations {
		ordered = append(ordered, operation)
	}

	sort.Slice(ordered, func(i, j int) bool {
		left := pathDepth(ordered[i].path)

		right := pathDepth(ordered[j].path)
		if left != right {
			return left < right
		}

		return ordered[i].path < ordered[j].path
	})

	return ordered
}

//nolint:wsl_v5 // Mount construction keeps each ordering constraint adjacent to its mutation.
func buildShieldMountOperations(policy processPolicy, mountPoints []string) []shieldMountOperation {
	mounts := make(map[string]shieldMountOperation)
	add := func(source, target string, readOnly bool) {
		if existing, ok := mounts[target]; ok && !existing.readOnly {
			return
		}
		mounts[target] = shieldMountOperation{source: source, target: target, readOnly: readOnly}
	}

	add(policy.projectRoot, policy.projectRoot, false)
	add(policy.projectRoot, policy.workDir, false)
	for _, mount := range policy.readMounts {
		add(mount.source, mount.target, true)
	}

	bases := make([]shieldMountOperation, 0, len(mounts))
	for _, mount := range mounts {
		bases = append(bases, mount)
	}
	for _, base := range bases {
		for _, mountPoint := range mountPoints {
			if mountPoint == base.source || !pathWithinRoot(mountPoint, base.source) {
				continue
			}
			rel, err := filepath.Rel(base.source, mountPoint)
			if err != nil {
				continue
			}
			add(mountPoint, filepath.Join(base.target, rel), true)
		}
	}

	ordered := make([]shieldMountOperation, 0, len(mounts))
	for _, mount := range mounts {
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

//nolint:wsl_v5 // Parent collection and depth ordering form one mount preparation pass.
func shieldMountDirectories(mounts []shieldMountOperation) []string {
	seen := map[string]struct{}{"/": {}}
	for _, mount := range mounts {
		for dir := filepath.Dir(mount.target); dir != "/" && dir != "."; dir = filepath.Dir(dir) {
			seen[dir] = struct{}{}
		}
	}

	dirs := make([]string, 0, len(seen)-1)
	for dir := range seen {
		if dir != "/" {
			dirs = append(dirs, dir)
		}
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
