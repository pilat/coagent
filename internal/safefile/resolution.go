package safefile

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/pilat/coagent/internal/coagenthome"
)

func (a *access) resolveHost(name string) (Path, error) {
	resolved, err := resolveHostPath(a.workDir, name)
	if err != nil {
		return Path{}, err
	}

	canonical, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return Path{}, fmt.Errorf("resolve path %s: %w", resolved, err)
	}

	return Path{Display: resolved, Canonical: canonical}, nil
}

//nolint:wsl_v5 // Display and canonical-relative forms are validated together.
func (a *access) projectRelative(name string) (string, string, error) {
	resolved, err := resolveHostPath(a.workDir, name)
	if err != nil {
		return "", "", err
	}

	rel, ok := relativeWithin(a.workDir, resolved)
	if !ok {
		rel, ok = relativeWithin(a.canonicalRoot, resolved)
	}
	if !ok {
		return "", "", outsideError(name)
	}

	return rel, resolved, nil
}

//nolint:wsl_v5 // Symlink traversal is bounded and restarts from the held root.
func (a *access) resolveProjectRelative(name string) (string, error) {
	remaining := filepath.Clean(name)
	if remaining == "." {
		return ".", nil
	}

	for range 255 {
		parts := strings.Split(remaining, string(filepath.Separator))
		resolved := make([]string, 0, len(parts))
		restarted := false

		for index, part := range parts {
			candidate := filepath.Join(append(resolved, part)...)
			info, err := a.root.Lstat(candidate)
			if err != nil {
				return "", fmt.Errorf("inspect project path %s: %w", candidate, err)
			}
			if info.Mode()&fs.ModeSymlink == 0 {
				resolved = append(resolved, part)
				continue
			}

			target, err := a.root.Readlink(candidate)
			if err != nil {
				return "", fmt.Errorf("read project symlink %s: %w", candidate, err)
			}
			remaining, err = a.joinSymlinkTarget(resolved, target, parts[index+1:])
			if err != nil {
				return "", err
			}
			restarted = true
			break
		}

		if !restarted {
			return filepath.Join(resolved...), nil
		}
	}

	return "", errors.New("project path has too many symbolic links")
}

//nolint:wsl_v5 // Relative and absolute link targets converge on one project-relative path.
func (a *access) joinSymlinkTarget(parent []string, target string, rest []string) (string, error) {
	var combined string
	if filepath.IsAbs(target) {
		rel, ok := a.absoluteSymlinkTarget(target)
		if !ok {
			return "", outsideError(target)
		}
		combined = rel
	} else {
		combined = filepath.Join(append(parent, target)...)
	}
	combined = filepath.Join(append([]string{combined}, rest...)...)
	if rel, ok := relativeWithin(".", combined); ok {
		return rel, nil
	}

	return "", outsideError(target)
}

func (a *access) absoluteSymlinkTarget(target string) (string, bool) {
	canonical := filepath.Clean(target)
	if rel, ok := relativeWithin(a.canonicalRoot, canonical); ok {
		return rel, true
	}

	resolved, err := filepath.EvalSymlinks(canonical)
	if err != nil {
		return "", false
	}

	return relativeWithin(a.canonicalRoot, resolved)
}

func (a *access) resolveForOpenFile(name string, flag int) (Path, error) {
	if flag&os.O_CREATE == 0 {
		return a.Resolve(name)
	}

	return a.ResolveTarget(name)
}

//nolint:wsl_v5 // Existing ancestors are resolved before authorizing a missing target.
func (a *access) resolveProjectTarget(name string) (string, error) {
	parts := strings.Split(filepath.Clean(name), string(filepath.Separator))
	for index := len(parts); index >= 0; index-- {
		prefix := filepath.Join(parts[:index]...)
		if prefix == "" {
			prefix = "."
		}
		resolved, err := a.resolveProjectRelative(prefix)
		if err == nil {
			return filepath.Join(append([]string{resolved}, parts[index:]...)...), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}

	return "", fmt.Errorf("resolve project target %s", name)
}

//nolint:wsl_v5 // Home expansion and absolute normalization form one host-readable path.
func resolveHostPath(workDir, name string) (string, error) {
	if name == "~" || strings.HasPrefix(name, "~/") {
		home, err := coagenthome.UserHome()
		if err != nil {
			return "", fmt.Errorf("resolve home path: %w", err)
		}
		name = filepath.Join(home, strings.TrimPrefix(name, "~/"))
	}
	if !filepath.IsAbs(name) {
		name = filepath.Join(workDir, name)
	}

	return filepath.Clean(name), nil
}

func relativeWithin(root, name string) (string, bool) {
	rel, err := filepath.Rel(root, name)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}

	return rel, true
}

func outsideError(name string) error {
	return fmt.Errorf("%w: %s: %s", ErrOutsideProject, name, ShieldDeniedMessage)
}

//nolint:wsl_v5 // Deferred reads reopen the same root and path as one operation.
func ReadFileAtRoot(root, rootIdentity, canonicalPath string) ([]byte, error) {
	if root == "" {
		content, err := os.ReadFile(canonicalPath)
		if err != nil {
			return nil, fmt.Errorf("read image: %w", err)
		}

		return content, nil
	}

	rel, ok := relativeWithin(root, canonicalPath)
	if !ok {
		return nil, outsideError(canonicalPath)
	}
	opened, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open image read root: %w", err)
	}
	defer func() { _ = opened.Close() }()
	info, err := opened.Stat(".")
	if err != nil {
		return nil, fmt.Errorf("identify image read root: %w", err)
	}
	if rootIdentity == "" || rootFileIdentity(info) != rootIdentity {
		return nil, outsideError(root)
	}

	content, err := opened.ReadFile(rel)
	if err != nil {
		return nil, fmt.Errorf("read rooted image: %w", err)
	}

	return content, nil
}
