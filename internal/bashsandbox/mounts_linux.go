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

func pathWithinRoot(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}

	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func pathDepth(path string) int {
	return strings.Count(filepath.Clean(path), string(filepath.Separator))
}
