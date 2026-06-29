package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/pilat/coagent/internal/tool"
)

const (
	applyPatchDescription = `Applies a unified diff patch to files.

Usage:
- Provide a standard unified diff format patch
- Supports multiple files in a single patch
- Creates new files if they don't exist
- Handles additions, deletions, and modifications

Format example:
--- a/file.txt
+++ b/file.txt
@@ -1,3 +1,4 @@
 line1
+new line
 line2
 line3`
)

var _ tool.Tool = (*applyPatchTool)(nil)

type applyPatchParams struct {
	Patch string `json:"patch"`
}

type applyPatchTool struct {
	workDir string
	mutator fileMutator
}

type patchFile struct {
	Path  string
	Hunks []patchHunk
}

type patchHunk struct {
	OldStart int
	OldCount int
	NewStart int
	NewCount int
	Lines    []patchLine
}

type patchLine struct {
	Type    byte // ' ', '+', '-'
	Content string
}

func newApplyPatchTool(workDir string, mutator fileMutator) *applyPatchTool {
	return &applyPatchTool{workDir: workDir, mutator: mutator}
}

func (t *applyPatchTool) ID() string          { return "apply_patch" }
func (t *applyPatchTool) Description() string { return applyPatchDescription }

func (t *applyPatchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"patch": {
				"type": "string",
				"description": "The unified diff patch to apply"
			}
		},
		"required": ["patch"]
	}`)
}

func (t *applyPatchTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p applyPatchParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	if p.Patch == "" {
		return nil, errors.New("patch is required")
	}

	files, err := parsePatch(p.Patch)
	if err != nil {
		return nil, fmt.Errorf("parse patch: %w", err)
	}

	if len(files) == 0 {
		return nil, errors.New("no files found in patch")
	}

	modified := make([]string, 0, len(files))

	for _, file := range files {
		filePath := resolvePath(t.workDir, file.Path)

		if err := applyFilePatches(ctx, t.mutator, filePath, file.Hunks); err != nil {
			return nil, fmt.Errorf("apply patch to %s: %w", file.Path, err)
		}

		modified = append(modified, file.Path)
	}

	return &tool.Result{
		Title:  fmt.Sprintf("Applied patch to %d file(s)", len(modified)),
		Output: "Modified files:\n" + strings.Join(modified, "\n"),
		Metadata: map[string]any{
			"files": modified,
		},
	}, nil
}

func parsePatch(patch string) ([]patchFile, error) {
	var files []patchFile
	var currentFile *patchFile
	var currentHunk *patchHunk

	scanner := bufio.NewScanner(strings.NewReader(patch))
	hunkHeaderRe := regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "--- ") {
			files = flushPatchFile(currentFile, currentHunk, files)
			currentFile = &patchFile{}
			currentHunk = nil

			continue
		}

		if strings.HasPrefix(line, "+++ ") {
			if currentFile != nil {
				currentFile.Path = parsePatchPath(line)
			}

			continue
		}

		if matches := hunkHeaderRe.FindStringSubmatch(line); matches != nil {
			if currentHunk != nil && currentFile != nil {
				currentFile.Hunks = append(currentFile.Hunks, *currentHunk)
			}

			currentHunk = parseHunkHeader(matches)

			continue
		}

		appendPatchLine(currentHunk, line)
	}

	if currentHunk != nil && currentFile != nil {
		currentFile.Hunks = append(currentFile.Hunks, *currentHunk)
	}

	if currentFile != nil && currentFile.Path != "" {
		files = append(files, *currentFile)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan patch: %w", err)
	}

	return files, nil
}

func flushPatchFile(currentFile *patchFile, currentHunk *patchHunk, files []patchFile) []patchFile {
	if currentFile != nil && currentHunk != nil {
		currentFile.Hunks = append(currentFile.Hunks, *currentHunk)
	}

	if currentFile != nil {
		files = append(files, *currentFile)
	}

	return files
}

func parsePatchPath(line string) string {
	path := strings.TrimPrefix(line, "+++ ")
	path = strings.TrimPrefix(path, "b/")
	path = strings.TrimPrefix(path, "a/")

	return path
}

func parseHunkHeader(matches []string) *patchHunk {
	oldStart, _ := strconv.Atoi(matches[1])
	oldCount := 1

	if matches[2] != "" {
		oldCount, _ = strconv.Atoi(matches[2])
	}

	newStart, _ := strconv.Atoi(matches[3])
	newCount := 1

	if matches[4] != "" {
		newCount, _ = strconv.Atoi(matches[4])
	}

	return &patchHunk{
		OldStart: oldStart,
		OldCount: oldCount,
		NewStart: newStart,
		NewCount: newCount,
	}
}

func appendPatchLine(hunk *patchHunk, line string) {
	if hunk == nil || line == "" {
		return
	}

	lineType := line[0]
	if lineType != ' ' && lineType != '+' && lineType != '-' {
		return
	}

	content := ""
	if len(line) > 1 {
		content = line[1:]
	}

	hunk.Lines = append(hunk.Lines, patchLine{Type: lineType, Content: content})
}

func applyFilePatches(
	ctx context.Context,
	mutator fileMutator,
	filePath string,
	hunks []patchHunk,
) error {
	unlock := lockFileWrite(filePath)
	defer unlock()

	var lines []string

	if err := rejectNonRegular(filePath); err != nil {
		return err
	}

	content, err := os.ReadFile(filePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read file: %w", err)
	}

	if err == nil {
		lines = strings.Split(string(content), "\n")
	}

	// Apply hunks in reverse order to preserve line numbers
	for _, v := range slices.Backward(hunks) {
		lines = applyHunk(lines, v)
	}

	result := strings.Join(lines, "\n")

	if err := mutator.WriteFile(ctx, filePath, []byte(result), true); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	return nil
}

func applyHunk(lines []string, hunk patchHunk) []string {
	var newLines []string

	for _, line := range hunk.Lines {
		switch line.Type {
		case ' ', '+':
			newLines = append(newLines, line.Content)
		}
	}

	startIdx := max(hunk.OldStart-1, 0)

	endIdx := startIdx + hunk.OldCount

	if startIdx > len(lines) {
		startIdx = len(lines)
	}

	if endIdx > len(lines) {
		endIdx = len(lines)
	}

	// Sized from the clamped indices, not from OldCount: a hunk header claiming more
	// lines than the file has would make the capacity negative and panic.
	result := make([]string, 0, startIdx+len(newLines)+len(lines)-endIdx)
	result = append(result, lines[:startIdx]...)
	result = append(result, newLines...)
	result = append(result, lines[endIdx:]...)

	return result
}
