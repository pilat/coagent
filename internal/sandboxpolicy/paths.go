package sandboxpolicy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/coagenthome"
)

// homePlaceholder stands in for a home directory that is only known when a
// policy is compiled. Validation works on the placeholder form so it never
// depends on the environment the daemon happens to run in.
const homePlaceholder = "/__coagent_home__"

// ExpandPath resolves a leading ~/ against the user's home directory.
func ExpandPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("path must not be empty")
	}

	if path != "~" && !strings.HasPrefix(path, "~/") {
		if strings.HasPrefix(path, "~") {
			return "", fmt.Errorf("path %q uses unsupported home expansion", path)
		}

		return path, nil
	}

	home, err := coagenthome.UserHome()
	if err != nil {
		return "", fmt.Errorf("resolve user home: %w", err)
	}

	if path == "~" {
		return home, nil
	}

	return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
}

// CanonicalProjectKey resolves a configured project key to the canonical root
// it selects. Matching is exact.
func CanonicalProjectKey(key string) (string, error) {
	anchor, err := normalizeRootAnchor(key)
	if err != nil {
		return "", err
	}

	return anchor, nil
}

// placeholderPath substitutes a leading ~ with an inert absolute prefix, so
// structural checks can run anywhere.
func placeholderPath(path string) (string, error) {
	if path == "" {
		return "", errors.New("path must not be empty")
	}

	if path == "~" {
		return homePlaceholder, nil
	}

	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		return filepath.Join(homePlaceholder, rest), nil
	}

	if strings.HasPrefix(path, "~") {
		return "", fmt.Errorf("path %q uses unsupported home expansion", path)
	}

	return path, nil
}

// rejectDotDot refuses traversal components: a declared anchor must name the
// object it appears to name.
func rejectDotDot(path string) error {
	if slices.Contains(strings.Split(path, string(filepath.Separator)), "..") {
		return fmt.Errorf("path %q contains a traversal component", path)
	}

	return nil
}

// normalizeAnchor resolves aliases before paths become security authority.
func normalizeAnchor(path string) (string, error) {
	if err := rejectDotDot(path); err != nil {
		return "", err
	}

	expanded, err := ExpandPath(path)
	if err != nil {
		return "", err
	}

	if !filepath.IsAbs(expanded) {
		return "", fmt.Errorf("path %q must be absolute or start with ~/", path)
	}

	return canonicalAnchor(filepath.Clean(expanded))
}

// normalizeRootAnchor is normalizeAnchor for paths that must address a real
// directory root, such as a canonical project root.
func normalizeRootAnchor(path string) (string, error) {
	anchor, err := normalizeAnchor(path)
	if err != nil {
		return "", err
	}

	if anchor == string(filepath.Separator) {
		return "", fmt.Errorf("path %q resolves to the filesystem root", path)
	}

	return anchor, nil
}

// canonicalAnchor resolves the longest existing prefix so an optional path
// cannot hide a protected parent behind a symlink.
func canonicalAnchor(anchor string) (string, error) {
	probe := anchor
	missing := make([]string, 0)

	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			for _, part := range slices.Backward(missing) {
				resolved = filepath.Join(resolved, part)
			}

			return filepath.Clean(resolved), nil
		}

		if !os.IsNotExist(err) || probe == "/" {
			return "", fmt.Errorf("resolve path %q: %w", anchor, err)
		}

		missing = append(missing, filepath.Base(probe))
		probe = filepath.Dir(probe)
	}
}

// probeObject reports the filesystem kind at an anchor. Absence is reported as
// (false, nil); a permission error is a real error, never a silent omission.
func probeObject(anchor string) (ObjectKind, bool, error) {
	info, err := os.Lstat(anchor)
	if os.IsNotExist(err) {
		return "", false, nil
	}

	if err != nil {
		return "", false, fmt.Errorf("inspect %q: %w", anchor, err)
	}

	if info.Mode()&os.ModeSymlink != 0 {
		return "", false, fmt.Errorf("path %q is a symlink and is not a mountable anchor", anchor)
	}

	if info.IsDir() {
		return KindDir, true, nil
	}

	if info.Mode().IsRegular() {
		return KindFile, true, nil
	}

	if info.Mode()&os.ModeSocket != 0 {
		return KindSocket, true, nil
	}

	return "", false, fmt.Errorf("path %q is neither a directory, regular file nor Unix socket", anchor)
}

// pathWithin reports whether path is root itself or lies beneath it.
func pathWithin(root, path string) bool {
	if root == path || root == string(filepath.Separator) {
		return true
	}

	if !strings.HasPrefix(path, root) {
		return false
	}

	return strings.HasPrefix(path[len(root):], string(filepath.Separator))
}
