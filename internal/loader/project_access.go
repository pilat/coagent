package loader

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//nolint:wsl_v5 // Both relative escape forms are part of one containment verdict.
func (s *svc) projectPath(workDir, path string) bool {
	root, err := filepath.Abs(workDir)
	if err != nil {
		return false
	}

	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (s *svc) readSourceFile(path string, project bool) ([]byte, error) {
	if !project || s.projectAccess == nil {
		return os.ReadFile(path) //nolint:wrapcheck // Caller adds source context.
	}

	opened, err := s.projectAccess.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open rooted project source: %w", err)
	}
	defer func() { _ = opened.File.Close() }()

	content, err := io.ReadAll(opened.File)
	if err != nil {
		return nil, fmt.Errorf("read rooted project source: %w", err)
	}

	return content, nil
}

func (s *svc) readSourceDir(path string, project bool) ([]fs.DirEntry, error) {
	if !project || s.projectAccess == nil {
		return os.ReadDir(path) //nolint:wrapcheck // Caller adds source context.
	}

	entries, _, err := s.projectAccess.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("read rooted project source directory: %w", err)
	}

	return entries, nil
}

func (s *svc) statSource(path string, project bool) (fs.FileInfo, error) {
	if !project || s.projectAccess == nil {
		return os.Stat(path) //nolint:wrapcheck // Caller treats absence as a skipped source.
	}

	info, _, err := s.projectAccess.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat rooted project source: %w", err)
	}

	return info, nil
}

func (s *svc) parseSourceFrontmatter(path string, project bool) ([]byte, string, error) {
	if !project || s.projectAccess == nil {
		return parseFrontmatterFile(path)
	}

	opened, err := s.projectAccess.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("open rooted project frontmatter: %w", err)
	}
	defer func() { _ = opened.File.Close() }()

	return parseFrontmatter(opened.File, path)
}
