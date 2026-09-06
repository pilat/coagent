package loader

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

const contextSeparator = "\n\n---\n\n"

// LoadAgentsMD reads and concatenates all AGENTS.md files.
// Files are concatenated in order with separator. Missing files are skipped.
// Returns empty string if no files are found.
func (s *svc) LoadAgentsMD(workDir string) (string, error) {
	paths := contextFilePaths(workDir)
	var contents []string

	for _, path := range paths {
		content, err := s.readSourceFile(path, s.projectPath(workDir, path))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}

			return "", fmt.Errorf("read context file %s: %w", path, err)
		}

		trimmed := strings.TrimSpace(string(content))
		if trimmed != "" {
			contents = append(contents, trimmed)
		}
	}

	return strings.Join(contents, contextSeparator), nil
}
