package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/safefile"
	"github.com/pilat/coagent/internal/tool"
)

const (
	grepMaxMatches     = 100
	grepMaxMatchesFile = 20
	grepDefaultContext = 0
	grepMaxFileSize    = 1024 * 1024 // 1MB

	grepDescription = `Searches for patterns in files using regex (regular expressions).

Usage:
- The pattern is a regular expression
- Search is recursive by default
- Results show file paths and matching lines with line numbers
- Use glob parameter to filter files (e.g., "*.go", "**/*.ts")
- Use context parameter to show surrounding lines
- Use ignore_case for case-insensitive matching
- Use files_only to only return matching file paths

CRITICAL: You can call multiple tools in a single response. When searching, run multiple grep/glob calls in parallel for optimal performance.

Limits:
- Maximum 100 total matches
- Maximum 20 matches per file
- Skips files larger than 1MB
- Skips binary files`
)

var _ tool.Tool = (*grepTool)(nil)

type grepParams struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path,omitempty"`
	Glob       string `json:"glob,omitempty"`
	Context    int    `json:"context,omitempty"`
	IgnoreCase bool   `json:"ignore_case,omitempty"`
	FilesOnly  bool   `json:"files_only,omitempty"`
}

type grepTool struct {
	workDir string
	access  safefile.Access
}

type grepMatch struct {
	file    string
	line    int
	content string
	context []string
}

func newGrepTool(workDir string) *grepTool {
	return &grepTool{workDir: workDir}
}

func newGrepToolWithAccess(workDir string, access safefile.Access) *grepTool {
	return &grepTool{workDir: workDir, access: access}
}

func (t *grepTool) ID() string          { return "grep" }
func (t *grepTool) ParallelSafe() bool  { return true }
func (t *grepTool) Description() string { return grepDescription }

func (t *grepTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"pattern": {
				"type": "string",
				"description": "The regex pattern to search for"
			},
			"path": {
				"type": "string",
				"description": "The directory to search in (defaults to working directory)"
			},
			"glob": {
				"type": "string",
				"description": "File pattern to filter (e.g., '*.go', '**/*.ts')"
			},
			"context": {
				"type": "integer",
				"description": "Number of context lines before and after each match"
			},
			"ignore_case": {
				"type": "boolean",
				"description": "Case-insensitive matching"
			},
			"files_only": {
				"type": "boolean",
				"description": "Only return file paths, not matching lines"
			}
		},
		"required": ["pattern"]
	}`)
}

func (t *grepTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	log := logger.Ctx(ctx).Named("tool.grep")

	p, re, err := t.parseGrepParams(params, log)
	if err != nil {
		return nil, err
	}

	searchPath, files, err := t.resolveFiles(p)
	if err != nil {
		return nil, err
	}

	contextLines := max(p.Context, 0)

	matches, matchingFiles, totalMatches := t.searchFiles(files, re, contextLines)
	truncated := totalMatches >= grepMaxMatches

	output := buildGrepOutput(p.FilesOnly, matches, matchingFiles, truncated)

	title := searchPath

	if t.workDir != "" {
		if rel, err := filepath.Rel(t.workDir, searchPath); err == nil {
			title = rel
		}
	}

	log.Debug("complete",
		zap.String("pattern", p.Pattern),
		zap.Int("matches", totalMatches),
		zap.Int("files", len(matchingFiles)),
		zap.Bool("truncated", truncated),
	)

	return &tool.Result{
		Title:  title,
		Output: strings.TrimSuffix(output, "\n"),
		Metadata: map[string]any{
			"matches":        totalMatches,
			"files":          len(matchingFiles),
			metaKeyTruncated: truncated,
		},
	}, nil
}

func (t *grepTool) parseGrepParams(params json.RawMessage, log *zap.Logger) (grepParams, *regexp.Regexp, error) {
	var p grepParams
	if err := json.Unmarshal(params, &p); err != nil {
		log.Warn("invalid_parameters", zap.Error(err))
		return p, nil, fmt.Errorf("invalid parameters: %w", err)
	}

	if p.Pattern == "" {
		log.Warn("empty_pattern")
		return p, nil, errors.New("pattern is required")
	}

	flags := ""
	if p.IgnoreCase {
		flags = "(?i)"
	}

	re, err := regexp.Compile(flags + p.Pattern)
	if err != nil {
		log.Warn("invalid_pattern", zap.String("pattern", p.Pattern), zap.Error(err))
		return p, nil, fmt.Errorf("invalid regex pattern: %w", err)
	}

	log.Debug("executing",
		zap.String("pattern", p.Pattern),
		zap.String("path", p.Path),
		zap.String("glob", p.Glob),
		zap.Bool("filesOnly", p.FilesOnly),
	)

	return p, re, nil
}

func (t *grepTool) resolveFiles(p grepParams) (string, []string, error) {
	if t.access != nil && t.access.Scope() == safefile.ProjectConfined {
		return t.resolveAccessFiles(p)
	}

	searchPath := p.Path
	if searchPath == "" {
		searchPath = t.workDir
	} else {
		searchPath = resolvePath(t.workDir, searchPath)
	}

	info, err := os.Stat(searchPath)
	if os.IsNotExist(err) {
		return "", nil, fmt.Errorf("path not found: %s", searchPath)
	}

	if err != nil {
		return "", nil, fmt.Errorf("stat path: %w", err)
	}

	var files []string

	if info.IsDir() {
		globPattern := "**/*"
		if p.Glob != "" {
			globPattern = p.Glob
		}

		files, _ = doublestar.FilepathGlob(filepath.Join(searchPath, globPattern))
	} else {
		// A FIFO/device/socket has no writer; os.Open blocks in the kernel,
		// uncancelable by ctx. stat (already done) never blocks.
		if !info.Mode().IsRegular() {
			return "", nil, fmt.Errorf("not a regular file: %s", searchPath)
		}

		files = []string{searchPath}
	}

	return searchPath, files, nil
}

//nolint:wsl_v5 // Rooted traversal and pattern admission form one search plan.
func (t *grepTool) resolveAccessFiles(p grepParams) (string, []string, error) {
	name := p.Path
	if name == "" {
		name = "."
	}
	info, path, err := t.access.Stat(name)
	if err != nil {
		return "", nil, fmt.Errorf("authorize grep path: %w", err)
	}
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return "", nil, fmt.Errorf("not a regular file: %s", path.Display)
		}
		return path.Display, []string{path.Display}, nil
	}

	pattern := "**/*"
	if p.Glob != "" {
		pattern = p.Glob
	}
	rooted, err := t.access.OpenRoot(path.Display)
	if err != nil {
		return "", nil, fmt.Errorf("open grep root: %w", err)
	}
	defer func() { _ = rooted.Root.Close() }()

	var files []string
	err = fs.WalkDir(rooted.Root.FS(), ".", func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil //nolint:nilerr // An inaccessible symlinked subtree is skipped.
		}
		if name == "." || entry.IsDir() {
			return nil
		}
		matched, err := doublestar.PathMatch(pattern, filepath.ToSlash(name))
		if err != nil {
			return fmt.Errorf("invalid glob pattern: %w", err)
		}
		if matched {
			files = append(files, filepath.Join(path.Display, filepath.FromSlash(name)))
		}
		return nil
	})
	if err != nil {
		return "", nil, fmt.Errorf("walk grep root: %w", err)
	}

	return path.Display, files, nil
}

//nolint:wsl_v5 // Per-file admission and bounded aggregation share one search pass.
func (t *grepTool) searchFiles(files []string, re *regexp.Regexp, contextLines int) ([]grepMatch, []string, int) {
	matches := make([]grepMatch, 0, len(files))
	matchingFiles := make([]string, 0, len(files))
	totalMatches := 0

	for _, file := range files {
		if totalMatches >= grepMaxMatches {
			break
		}

		// !IsRegular covers dirs and FIFOs/devices/sockets — os.Open on a writer-less
		// FIFO would block uncancelably in searchFile.
		var fileInfo fs.FileInfo
		var err error
		if t.access != nil && t.access.Scope() == safefile.ProjectConfined {
			fileInfo, _, err = t.access.Stat(file)
		} else {
			fileInfo, err = os.Stat(file)
		}
		if err != nil || !fileInfo.Mode().IsRegular() || fileInfo.Size() > grepMaxFileSize {
			continue
		}

		if t.fileIsBinary(file) {
			continue
		}

		fileMatches := t.searchFile(file, re, contextLines, grepMaxMatchesFile)
		if len(fileMatches) == 0 {
			continue
		}

		matchingFiles = append(matchingFiles, file)

		for _, m := range fileMatches {
			if totalMatches >= grepMaxMatches {
				break
			}

			matches = append(matches, m)
			totalMatches++
		}
	}

	return matches, matchingFiles, totalMatches
}

//nolint:wsl_v5 // Rooted handle lifetime covers the complete binary check.
func (t *grepTool) fileIsBinary(path string) bool {
	if t.access == nil || t.access.Scope() != safefile.ProjectConfined {
		return isBinaryFile(path)
	}
	opened, err := t.access.Open(path)
	if err != nil {
		return true
	}
	defer func() { _ = opened.File.Close() }()

	return isBinaryReader(opened.File, opened.Path.Canonical)
}

func buildGrepOutput(filesOnly bool, matches []grepMatch, matchingFiles []string, truncated bool) string {
	var output strings.Builder

	if filesOnly {
		if len(matchingFiles) == 0 {
			output.WriteString("No files matched")
		} else {
			for _, f := range matchingFiles {
				output.WriteString(f)
				output.WriteString("\n")
			}
		}

		return output.String()
	}

	if len(matches) == 0 {
		output.WriteString("No matches found")
		return output.String()
	}

	currentFile := ""
	for _, m := range matches {
		if m.file != currentFile {
			if currentFile != "" {
				output.WriteString("\n")
			}

			fmt.Fprintf(&output, "=== %s ===\n", m.file)
			currentFile = m.file
		}

		for _, ctx := range m.context {
			output.WriteString(ctx)
			output.WriteString("\n")
		}

		fmt.Fprintf(&output, "%d: %s\n", m.line, m.content)
	}

	if truncated {
		fmt.Fprintf(&output, "\n(Results truncated at %d matches)", grepMaxMatches)
	}

	return output.String()
}

//nolint:wsl_v5 // Rooted and host-readable paths converge on one scanner.
func (t *grepTool) searchFile(path string, re *regexp.Regexp, contextLines, maxMatches int) []grepMatch {
	var file *os.File
	var err error
	if t.access != nil && t.access.Scope() == safefile.ProjectConfined {
		opened, openErr := t.access.Open(path)
		if openErr != nil {
			return nil
		}
		file = opened.File
	} else {
		file, err = os.Open(path)
	}
	if err != nil {
		return nil
	}

	defer func() { _ = file.Close() }()

	return searchReader(file, path, re, contextLines, maxMatches)
}

func searchReader(
	file *os.File,
	path string,
	re *regexp.Regexp,
	contextLines, maxMatches int,
) []grepMatch {
	var lines []string

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	var matches []grepMatch
	for i, line := range lines {
		if len(matches) >= maxMatches {
			break
		}

		if re.MatchString(line) {
			var ctx []string

			if contextLines > 0 {
				start := max(i-contextLines, 0)

				for j := start; j < i; j++ {
					ctx = append(ctx, fmt.Sprintf("%d- %s", j+1, lines[j]))
				}
			}

			matches = append(matches, grepMatch{
				file:    path,
				line:    i + 1,
				content: line,
				context: ctx,
			})
		}
	}

	return matches
}
