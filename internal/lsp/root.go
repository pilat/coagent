package lsp

import (
	"os"
	"path/filepath"
	"strings"
)

func findNearestRoot(workDir, startFile string, targets []string) string {
	dir := filepath.Dir(startFile)
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(workDir, dir)
	}

	for {
		for _, target := range targets {
			if _, err := os.Stat(filepath.Join(dir, target)); err == nil {
				return dir
			}
		}

		parent := filepath.Dir(dir)
		if parent == dir || !strings.HasPrefix(dir, workDir) {
			return workDir
		}

		dir = parent
	}
}
