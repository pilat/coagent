package loader

import (
	"fmt"
	"io"
	"io/fs"
	"os"

	"github.com/pilat/coagent/internal/safefile"
)

// readSourceFile reads one source file. A nil access marks a trusted host
// input; project sources read through the project-confined root.
func readSourceFile(path string, access safefile.Access) ([]byte, error) {
	if access == nil {
		return os.ReadFile(path) //nolint:wrapcheck // Caller adds source context.
	}

	opened, err := access.Open(path)
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

func readSourceDir(path string, access safefile.Access) ([]fs.DirEntry, error) {
	if access == nil {
		return os.ReadDir(path) //nolint:wrapcheck // Caller adds source context.
	}

	entries, _, err := access.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("read rooted project source directory: %w", err)
	}

	return entries, nil
}

func statSource(path string, access safefile.Access) (fs.FileInfo, error) {
	if access == nil {
		return os.Stat(path) //nolint:wrapcheck // Caller treats absence as a skipped source.
	}

	info, _, err := access.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat rooted project source: %w", err)
	}

	return info, nil
}

func parseSourceFrontmatter(path string, access safefile.Access) ([]byte, string, error) {
	if access == nil {
		return parseFrontmatterFile(path)
	}

	opened, err := access.Open(path)
	if err != nil {
		return nil, "", fmt.Errorf("open rooted project frontmatter: %w", err)
	}
	defer func() { _ = opened.File.Close() }()

	return parseFrontmatter(opened.File, path)
}

// sourceAccesses opens one project-confined root per distinct non-empty source
// root; a failed root is recorded so the caller drops only its own sources.
func sourceAccesses(sources []sourceInfo) (map[string]safefile.Access, []error) {
	roots := make(map[string]struct{})

	for _, src := range sources {
		if src.root != "" {
			roots[src.root] = struct{}{}
		}
	}

	var errs []error
	accesses := make(map[string]safefile.Access, len(roots))

	for root := range roots {
		access, err := safefile.New(safefile.ProjectPolicy(root), root)
		if err != nil {
			errs = append(errs, fmt.Errorf("contain sources rooted at %s: %w", root, err))
			continue
		}

		accesses[root] = access
	}

	return accesses, errs
}

func closeAccesses(accesses map[string]safefile.Access) {
	for _, access := range accesses {
		_ = access.Close()
	}
}
