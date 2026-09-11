package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"
	"unicode/utf8"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/safefile"
	"github.com/pilat/coagent/internal/tool"
)

const (
	defaultTailLines = 50
	maxTailLines     = 2000
	tailMaxBytes     = 50 * 1024
	tailNotice       = "\n\n(tail truncated to fit the byte limit)"

	tailDescription = `Reads the final lines of a text file efficiently from the end without scanning the complete file.

Usage:
- The file_path parameter can be absolute or relative to the working directory
- lines defaults to 50, must be positive, and is capped at 2000
- The result is capped at 50 KiB; when complete final lines exceed it, the largest final whole-line suffix that fits is returned with a truncation notice
- An individually long line uses read's line truncation
- Rejects binary and non-regular files
- Use tail for deliberate output inspection when diagnostics are needed; do not repeat it as a polling loop because final results arrive automatically`
)

var _ tool.Tool = (*tailTool)(nil)

type tailParams struct {
	FilePath string `json:"file_path"`
	Lines    *int   `json:"lines,omitempty"`
}

type tailTool struct {
	workDir string
	access  safefile.Access
}

func newTailTool(workDir string, access safefile.Access) *tailTool {
	return &tailTool{workDir: workDir, access: access}
}

func (t *tailTool) ID() string          { return "tail" }
func (t *tailTool) ParallelSafe() bool  { return true }
func (t *tailTool) Description() string { return tailDescription }

func (t *tailTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"file_path": {
				"type": "string",
				"description": "The path to the file to read from the end"
			},
			"lines": {
				"type": "integer",
				"description": "The number of final lines to read (defaults to 50, max 2000)"
			}
		},
		"required": ["file_path"]
	}`)
}

func (t *tailTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	log := logger.Ctx(ctx).Named("tool.tail")

	var p tailParams
	if err := json.Unmarshal(params, &p); err != nil {
		log.Warn("invalid_parameters", zap.Error(err))

		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	if p.FilePath == "" {
		return nil, errors.New("file_path is required")
	}

	lines, err := tailLines(p.Lines)
	if err != nil {
		return nil, err
	}

	filePath := resolvePath(t.workDir, p.FilePath)

	lockPath := filePath
	if t.access != nil {
		resolved, err := t.access.Resolve(filePath)
		if err != nil {
			return nil, fmt.Errorf("authorize read path: %w", err)
		}

		lockPath = resolved.Canonical
	}

	unlock := lockFileRead(lockPath)
	defer unlock()

	file, info, err := t.openRegularFile(filePath)
	if err != nil {
		return nil, err
	}

	defer file.Close()

	output, truncated, err := tailFromFile(file, info.Size(), lines, filePath)
	if err != nil {
		return nil, err
	}

	if truncated {
		output += tailNotice
	}

	return &tool.Result{
		Title:  relativeTitle(t.workDir, filePath),
		Output: output,
		Metadata: map[string]any{
			metaKeyPath:      filePath,
			metaKeyTruncated: truncated,
		},
	}, nil
}

func tailLines(requested *int) (int, error) {
	if requested == nil {
		return defaultTailLines, nil
	}

	if *requested <= 0 {
		return 0, errors.New("lines must be positive")
	}

	return min(*requested, maxTailLines), nil
}

func (t *tailTool) openRegularFile(filePath string) (*os.File, os.FileInfo, error) {
	file, err := t.openFile(filePath)
	if err != nil {
		return nil, nil, err
	}

	info, err := file.Stat()
	if err != nil {
		_ = file.Close()

		return nil, nil, fmt.Errorf("stat file: %w", err)
	}

	if !info.Mode().IsRegular() {
		_ = file.Close()

		return nil, nil, errors.New("tail requires a regular file")
	}

	return file, info, nil
}

func (t *tailTool) openFile(filePath string) (*os.File, error) {
	flags := os.O_RDONLY | syscall.O_NONBLOCK

	if t.access == nil {
		file, err := os.OpenFile(filePath, flags, 0)
		if err != nil {
			return nil, fmt.Errorf("open file: %w", err)
		}

		return file, nil
	}

	opened, err := t.access.OpenFile(filePath, flags, 0)
	if err != nil {
		return nil, fmt.Errorf("authorize read path: %w", err)
	}

	return opened.File, nil
}

// tailFromFile reads backward in blocks and assembles the bounded final lines
// in file order.
func tailFromFile(file *os.File, size int64, lines int, filePath string) (string, bool, error) {
	if size == 0 {
		return "(no output)", false, nil
	}

	if isBinaryReader(file, filePath) {
		return "", false, errors.New("tail requires a text file")
	}

	collected, full, err := backgroundprocess.ScanTailLines(
		file, size, lines, tailMaxBytes-int64(len(tailNotice)), maxLineLength,
	)
	if err != nil {
		return "", false, fmt.Errorf("scan file tail: %w", err)
	}

	var out strings.Builder

	for i, line := range collected {
		if i > 0 {
			out.WriteByte('\n')
		}

		out.Write(line)
	}

	if !utf8.ValidString(out.String()) {
		return "", false, errors.New("tail requires valid UTF-8 text")
	}

	return out.String(), full, nil
}
