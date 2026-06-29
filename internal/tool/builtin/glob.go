package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/bmatcuk/doublestar/v4"

	"github.com/pilat/coagent/internal/tool"
)

const (
	globLimit = 100

	globDescription = `Finds files matching a glob pattern.

Usage:
- Supports standard glob patterns including ** for recursive matching
- Examples: "**/*.go", "src/**/*.ts", "*.md"
- Returns matching file paths sorted by modification time (newest first)
- Results are limited to 100 files
- The path parameter can be absolute or relative to the working directory
- Directories are excluded from results

CRITICAL: You can call multiple tools in a single response. When searching, run multiple glob/grep calls in parallel for optimal performance.`
)

var _ tool.Tool = (*globTool)(nil)

type globParams struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
}

type globTool struct {
	workDir string
}

type fileMatch struct {
	path  string
	mtime int64
}

func newGlobTool(workDir string) *globTool {
	return &globTool{workDir: workDir}
}

func (t *globTool) ID() string          { return "glob" }
func (t *globTool) Description() string { return globDescription }

func (t *globTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"pattern": {
				"type": "string",
				"description": "The glob pattern to match files against (e.g., '**/*.go', 'src/**/*.ts')"
			},
			"path": {
				"type": "string",
				"description": "The directory to search in. If not specified, the working directory will be used."
			}
		},
		"required": ["pattern"]
	}`)
}

func (t *globTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p globParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	if p.Pattern == "" {
		return nil, errors.New("pattern is required")
	}

	searchPath, err := t.resolveSearchPath(p.Path)
	if err != nil {
		return nil, err
	}

	files, truncated, err := t.findFiles(searchPath, p.Pattern)
	if err != nil {
		return nil, err
	}

	output := buildGlobOutput(files, truncated)

	title := searchPath

	if t.workDir != "" {
		if rel, err := filepath.Rel(t.workDir, searchPath); err == nil {
			title = rel
		}
	}

	return &tool.Result{
		Title:  title,
		Output: strings.TrimSuffix(output, "\n"),
		Metadata: map[string]any{
			metaKeyCount:     len(files),
			metaKeyTruncated: truncated,
			"pattern":        p.Pattern,
		},
	}, nil
}

func (t *globTool) resolveSearchPath(path string) (string, error) {
	if path == "" {
		return t.workDir, nil
	}

	searchPath := resolvePath(t.workDir, path)
	info, err := os.Stat(searchPath)

	if os.IsNotExist(err) {
		return "", fmt.Errorf("path not found: %s", searchPath)
	}

	if err != nil {
		return "", fmt.Errorf("stat path: %w", err)
	}

	if !info.IsDir() {
		return "", fmt.Errorf("path is not a directory: %s", searchPath)
	}

	return searchPath, nil
}

func (t *globTool) findFiles(searchPath, pattern string) ([]fileMatch, bool, error) {
	matches, err := doublestar.FilepathGlob(filepath.Join(searchPath, pattern))
	if err != nil {
		return nil, false, fmt.Errorf("invalid glob pattern: %w", err)
	}

	files := make([]fileMatch, 0, len(matches))

	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil || info.IsDir() {
			continue
		}

		files = append(files, fileMatch{path: match, mtime: info.ModTime().UnixNano()})
	}

	sort.Slice(files, func(i, j int) bool {
		return files[i].mtime > files[j].mtime
	})

	truncated := len(files) > globLimit
	if truncated {
		files = files[:globLimit]
	}

	return files, truncated, nil
}

func buildGlobOutput(files []fileMatch, truncated bool) string {
	var output strings.Builder
	if len(files) == 0 {
		output.WriteString("No files found")
		return output.String()
	}

	for _, f := range files {
		output.WriteString(f.path)
		output.WriteString("\n")
	}

	if truncated {
		output.WriteString("\n(Results are truncated. Consider using a more specific path or pattern.)")
	}

	return output.String()
}
