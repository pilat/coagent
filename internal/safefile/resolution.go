package safefile

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/pilat/coagent/internal/coagenthome"
)

const maxSymlinkRestarts = 255

func resolveHost(workDir, name string) (Path, error) {
	resolved, err := resolveHostPath(workDir, name)
	if err != nil {
		return Path{}, err
	}

	canonical, err := filepath.EvalSymlinks(resolved)
	if err != nil {
		return Path{}, fmt.Errorf("resolve path %s: %w", resolved, err)
	}

	return Path{Display: resolved, Canonical: canonical}, nil
}

// resolveGranted maps a requested name onto the deepest grant that admits it
// and resolves the remainder inside that grant's held root. Virtual temporary
// paths are ordinary targets here: /tmp is a grant whose source is the
// project's private backing directory, so the host namesake is never read.
func (a *access) resolveGranted(name string, allowMissing bool) (Path, error) {
	display, err := resolveHostPath(a.workDir, name)
	if err != nil {
		return Path{}, err
	}

	held, relative, ok := a.matchTarget(display)
	if !ok {
		return Path{}, outsideError(name)
	}

	if held.file {
		return Path{
			Display: display, Canonical: held.root, ReadRoot: held.root,
			ReadRootID: held.identity, Relative: ".", Writable: held.writable,
		}, nil
	}

	resolved, err := a.resolveWithinGrant(held, relative, allowMissing)
	if err != nil {
		return Path{}, err
	}

	return Path{
		Display: display, Canonical: filepath.Join(held.root, resolved),
		ReadRoot: held.root, ReadRootID: held.identity, Relative: resolved, Writable: held.writable,
	}, nil
}

func (a *access) resolveWithinGrant(held grant, name string, allowMissing bool) (string, error) {
	if allowMissing {
		return resolveGrantTarget(held, name)
	}

	return resolveGrantRelative(held, name)
}

//nolint:wsl_v5 // Symlink traversal is bounded and restarts from the held root.
func resolveGrantRelative(held grant, name string) (string, error) {
	remaining := filepath.Clean(name)
	if remaining == "." {
		return ".", nil
	}

	for range maxSymlinkRestarts {
		parts := strings.Split(remaining, string(filepath.Separator))
		resolved := make([]string, 0, len(parts))
		restarted := false

		for index, part := range parts {
			candidate := filepath.Join(append(resolved, part)...)
			info, err := held.handle.Lstat(candidate)
			if err != nil {
				return "", fmt.Errorf("inspect granted path %s: %w", candidate, err)
			}

			if info.Mode()&fs.ModeSymlink == 0 {
				resolved = append(resolved, part)

				continue
			}

			target, err := held.handle.Readlink(candidate)
			if err != nil {
				return "", fmt.Errorf("read granted symlink %s: %w", candidate, err)
			}

			remaining, err = joinSymlinkTarget(held, resolved, target, parts[index+1:])
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

	return "", errors.New("granted path has too many symbolic links")
}

func joinSymlinkTarget(held grant, parent []string, target string, rest []string) (string, error) {
	var combined string

	if filepath.IsAbs(target) {
		relative, ok := absoluteSymlinkTarget(held, target)
		if !ok {
			return "", outsideError(target)
		}

		combined = relative
	} else {
		combined = filepath.Join(append(parent, target)...)
	}

	combined = filepath.Join(append([]string{combined}, rest...)...)
	if relative, ok := relativeWithin(".", combined); ok {
		return relative, nil
	}

	return "", outsideError(target)
}

// absoluteSymlinkTarget refuses a link that leaves its own grant, so a symlink
// cannot widen authority onto another root.
func absoluteSymlinkTarget(held grant, target string) (string, bool) {
	if relative, ok := relativeWithin(held.root, filepath.Clean(target)); ok {
		return relative, true
	}

	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", false
	}

	return relativeWithin(held.root, resolved)
}

func resolveGrantTarget(held grant, name string) (string, error) {
	parts := strings.Split(filepath.Clean(name), string(filepath.Separator))

	for index := len(parts); index >= 0; index-- {
		prefix := filepath.Join(parts[:index]...)
		if prefix == "" {
			prefix = "."
		}

		resolved, err := resolveGrantRelative(held, prefix)
		if err == nil {
			return filepath.Join(append([]string{resolved}, parts[index:]...)...), nil
		}

		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}

	return "", fmt.Errorf("resolve granted target %s", name)
}

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

// ReadFileAtRoot reopens the same root and path as one deferred read. Callers
// must have re-authorized the reference through Access.AuthorizeRead first.
func ReadFileAtRoot(root, rootIdentity, canonicalPath string) ([]byte, error) {
	if root == "" {
		content, err := os.ReadFile(canonicalPath)
		if err != nil {
			return nil, fmt.Errorf("read image: %w", err)
		}

		return content, nil
	}

	if root == canonicalPath {
		file, err := os.Open(root)
		if err != nil {
			return nil, fmt.Errorf("open granted image: %w", err)
		}
		defer func() { _ = file.Close() }()

		info, err := file.Stat()
		if err != nil || !info.Mode().IsRegular() || rootFileIdentity(info) != rootIdentity {
			return nil, outsideError(root)
		}

		content, err := io.ReadAll(file)
		if err != nil {
			return nil, fmt.Errorf("read granted file %q: %w", canonicalPath, err)
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
