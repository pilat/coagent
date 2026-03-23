package loader

import (
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
		content, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
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
