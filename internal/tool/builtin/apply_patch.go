package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/pilat/coagent/internal/safefile"
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
 line3

Editing a file also counts as having read it for a later write.`
)

var _ tool.Tool = (*applyPatchTool)(nil)

type applyPatchParams struct {
	Patch string `json:"patch"`
}

type applyPatchTool struct {
	workDir string
	mutator fileMutator
	access  safefile.Access
	tracker FileReadTracker
}

func newApplyPatchToolWithAccess(
	workDir string,
	access safefile.Access,
	mutator fileMutator,
	tracker FileReadTracker,
) *applyPatchTool {
	return &applyPatchTool{workDir: workDir, access: access, mutator: mutator, tracker: tracker}
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
func (t *applyPatchTool) ParallelSafe() bool  { return false }
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

	paths := make([]string, len(files))

	seenPaths := make(map[string]struct{}, len(files))
	for i, file := range files {
		filePath, err := resolveAccessTarget(t.access, t.workDir, file.Path)
		if err != nil {
			return nil, err
		}

		if _, duplicate := seenPaths[filePath]; duplicate {
			return nil, fmt.Errorf("duplicate file in patch: %s", file.Path)
		}

		seenPaths[filePath] = struct{}{}
		paths[i] = filePath
	}

	unlock := lockFileWrites(paths)
	defer unlock()

	plans := make([]filePatchPlan, len(files))
	for i, file := range files {
		plan, err := planFilePatches(t.access, paths[i], file.Hunks)
		if err != nil {
			return nil, fmt.Errorf("apply patch to %s: %w", file.Path, err)
		}

		plans[i] = plan
	}

	modified := make([]string, 0, len(files))
	for i, file := range files {
		if err := writeFilePatch(ctx, t.access, t.mutator, paths[i], plans[i].content); err != nil {
			t.rollback(ctx, paths, plans, i)
			return nil, fmt.Errorf("apply patch to %s: %w", file.Path, err)
		}

		if err := t.refreshRead(ctx, paths[i]); err != nil {
			t.rollback(ctx, paths, plans, i)
			return nil, fmt.Errorf("record patch to %s: %w", file.Path, err)
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

func (t *applyPatchTool) rollback(
	ctx context.Context,
	paths []string,
	plans []filePatchPlan,
	last int,
) {
	for i := last; i >= 0; i-- {
		plan := plans[i]
		if plan.exists {
			_ = t.mutator.WriteFile(ctx, paths[i], plan.original, true)
			continue
		}

		if t.access == nil {
			_ = os.Remove(paths[i])
		}
	}
}

//nolint:wsl_v5 // Refresh is the post-mutation ledger boundary.
func (t *applyPatchTool) refreshRead(ctx context.Context, filePath string) error {
	if t.tracker == nil {
		return nil
	}
	key, record, err := recordFingerprint(t.access, t.workDir, filePath)
	if err != nil {
		return err
	}
	if err := t.tracker.RecordRead(ctx, key, record); err != nil {
		return fmt.Errorf("record patch file read: %w", err)
	}
	return nil
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
	if tab := strings.IndexByte(path, '\t'); tab >= 0 {
		path = path[:tab]
	}

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

type filePatchPlan struct {
	content  []byte
	original []byte
	exists   bool
}

func applyFilePatches(
	ctx context.Context,
	access safefile.Access,
	mutator fileMutator,
	filePath string,
	hunks []patchHunk,
) error {
	unlock := lockFileWrite(filePath)
	defer unlock()

	plan, err := planFilePatches(access, filePath, hunks)
	if err != nil {
		return err
	}

	return writeFilePatch(ctx, access, mutator, filePath, plan.content)
}

func lockFileWrites(paths []string) func() {
	keys := append([]string(nil), paths...)
	sort.Strings(keys)
	keys = uniqueStrings(keys)

	unlocks := make([]func(), 0, len(keys))
	for _, path := range keys {
		unlocks = append(unlocks, lockFileWrite(path))
	}

	return func() {
		for _, unlock := range slices.Backward(unlocks) {
			unlock()
		}
	}
}

func uniqueStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}

	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}

	return out
}

func planFilePatches(access safefile.Access, filePath string, hunks []patchHunk) (filePatchPlan, error) {
	if err := rejectNonRegularAccess(access, filePath); err != nil {
		return filePatchPlan{}, err
	}

	content, err := readAccessFile(access, filePath)

	exists := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return filePatchPlan{}, fmt.Errorf("read file: %w", err)
	}

	var lines []string
	if exists {
		lines = strings.Split(string(content), "\n")
	}

	lineDelta := 0
	for _, hunk := range hunks {
		updated, delta, err := applyHunkAt(lines, hunk, hunk.OldStart-1+lineDelta, !exists)
		if err != nil {
			return filePatchPlan{}, err
		}

		lines = updated
		lineDelta += delta
	}

	return filePatchPlan{
		content:  []byte(strings.Join(lines, "\n")),
		original: append([]byte(nil), content...),
		exists:   exists,
	}, nil
}

func writeFilePatch(
	ctx context.Context,
	access safefile.Access,
	mutator fileMutator,
	filePath string,
	content []byte,
) error {
	if err := mutator.WriteFile(ctx, filePath, content, true); err != nil {
		return fmt.Errorf("write file: %w", err)
	}

	written, err := readAccessFile(access, filePath)
	if err != nil {
		return fmt.Errorf("verify file: read written file: %w", err)
	}

	if !bytes.Equal(written, content) {
		return errors.New("verify file: written content does not match patch result")
	}

	return nil
}

func applyHunk(lines []string, hunk patchHunk) []string {
	updated, _, err := applyHunkAt(lines, hunk, hunk.OldStart-1, false)
	if err != nil {
		return lines
	}

	return updated
}

func applyHunkAt(lines []string, hunk patchHunk, hint int, missingFile bool) ([]string, int, error) {
	oldLines := make([]string, 0, hunk.OldCount)

	newLines := make([]string, 0, hunk.NewCount)
	for _, line := range hunk.Lines {
		switch line.Type {
		case ' ':
			oldLines = append(oldLines, line.Content)
			newLines = append(newLines, line.Content)
		case '-':
			oldLines = append(oldLines, line.Content)
		case '+':
			newLines = append(newLines, line.Content)
		}
	}

	if len(oldLines) != hunk.OldCount || len(newLines) != hunk.NewCount {
		return nil, 0, errors.New("hunk line counts do not match header")
	}

	if len(oldLines) == 0 {
		if missingFile && len(hunk.Lines) != len(newLines) {
			return nil, 0, errors.New("context-bearing hunk cannot create a missing file")
		}

		if hint < 0 {
			hint = 0
		}

		if hint > len(lines) {
			hint = len(lines)
		}

		return replaceLineRange(lines, hint, hint, newLines), len(newLines), nil
	}

	start, err := findHunkAnchor(lines, oldLines, hint)
	if err != nil {
		return nil, 0, err
	}

	return replaceLineRange(lines, start, start+len(oldLines), newLines), len(newLines) - len(oldLines), nil
}

func findHunkAnchor(lines, oldLines []string, hint int) (int, error) {
	type candidate struct{ index, exact, whitespace, distance int }
	var candidates []candidate

	for i := 0; i+len(oldLines) <= len(lines); i++ {
		exact, whitespace := 0, 0
		matched := true

		for j, want := range oldLines {
			got := lines[i+j]
			switch {
			case got == want:
				exact++
			case strings.TrimRight(got, " \t") == strings.TrimRight(want, " \t"):
				whitespace++
			default:
				matched = false
			}

			if !matched {
				break
			}
		}

		if matched {
			candidates = append(candidates, candidate{i, exact, whitespace, abs(i - hint)})
		}
	}

	if len(candidates) == 0 {
		return 0, errors.New("hunk old block not found in running file content")
	}

	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.exact != b.exact {
			return a.exact > b.exact
		}

		if a.whitespace != b.whitespace {
			return a.whitespace > b.whitespace
		}

		return a.distance < b.distance
	})

	if len(candidates) > 1 && candidates[0].exact == candidates[1].exact &&
		candidates[0].whitespace == candidates[1].whitespace &&
		candidates[0].distance == candidates[1].distance {
		return 0, errors.New("hunk old block is ambiguous in running file content")
	}

	return candidates[0].index, nil
}

func replaceLineRange(lines []string, start, end int, replacement []string) []string {
	result := make([]string, 0, len(lines)-(end-start)+len(replacement))
	result = append(result, lines[:start]...)
	result = append(result, replacement...)
	result = append(result, lines[end:]...)

	return result
}

func abs(value int) int {
	if value < 0 {
		return -value
	}

	return value
}
