package safefile

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pilat/coagent/internal/coagenthome"
)

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

// canonicalPrefix resolves symlinks along the longest existing prefix of
// path, keeping any non-existent tail as-is: a write target need not exist
// yet, but every real component it traverses must be resolved before the
// authority check, so a symlink cannot smuggle it into a denied directory.
func canonicalPrefix(path string) string {
	probe := path

	var missing []string

	for {
		resolved, err := filepath.EvalSymlinks(probe)
		if err == nil {
			for _, part := range slices.Backward(missing) {
				resolved = filepath.Join(resolved, part)
			}

			return filepath.Clean(resolved)
		}

		if !os.IsNotExist(err) || probe == string(filepath.Separator) {
			return path
		}

		missing = append(missing, filepath.Base(probe))
		probe = filepath.Dir(probe)
	}
}

func relativeWithin(root, name string) (string, bool) {
	rel, err := filepath.Rel(root, name)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}

	return rel, true
}

// ReadFileAtRoot reopens the same root and path as one deferred read. Callers
// must have re-authorized the reference through Access.AuthorizeRead first.
func ReadFileAtRoot(root, rootIdentity, canonicalPath string) ([]byte, error) {
	if root == "" {
		file, err := openResolvedFile(nil, canonicalPath, os.O_RDONLY, 0)
		if err != nil {
			return nil, fmt.Errorf("open image: %w", err)
		}
		defer func() { _ = file.Close() }()

		content, err := io.ReadAll(file)
		if err != nil {
			return nil, fmt.Errorf("read image: %w", err)
		}

		return content, nil
	}

	if root == canonicalPath {
		file, err := openResolvedFile(nil, root, os.O_RDONLY, 0)
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

	file, err := openResolvedFile(opened, rel, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("open rooted image: %w", err)
	}
	defer func() { _ = file.Close() }()

	content, err := io.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read rooted image: %w", err)
	}

	return content, nil
}
