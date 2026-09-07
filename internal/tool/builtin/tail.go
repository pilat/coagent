package builtin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/safefile"
	"github.com/pilat/coagent/internal/tool"
)

const (
	defaultTailLines  = 50
	maxTailLines      = 2000
	tailMaxBytes      = 50 * 1024
	tailBlockReadSize = 8 * 1024

	tailDescription = `Reads the final lines of a text file efficiently from the end without scanning the complete file.

Usage:
- The file_path parameter can be absolute or relative to the working directory
- lines defaults to 50, must be positive, and is capped at 2000
- The result is capped at 50 KiB; when complete final lines exceed it, the largest final whole-line suffix that fits is returned with a truncation notice
- An individually long line uses read's line truncation
- Rejects binary and non-regular files
- Use this to inspect the tail of a background process output file; completion of background processes is delivered automatically, so tail is only for independent work`
)

var _ tool.Tool = (*tailTool)(nil)

type tailParams struct {
	FilePath string `json:"file_path"`
	Lines    int    `json:"lines,omitempty"`
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

	lines := p.Lines
	if lines <= 0 {
		lines = defaultTailLines
	}

	if lines > maxTailLines {
		lines = maxTailLines
	}

	filePath := resolvePath(t.workDir, p.FilePath)

	var (
		file *os.File
		err  error
	)

	if t.access != nil {
		var opened *safefile.Opened

		opened, err = t.access.Open(filePath)
		if err != nil {
			return nil, fmt.Errorf("authorize read path: %w", err)
		}

		file = opened.File
	} else {
		file, err = os.Open(filePath)
	}

	if err != nil {
		return nil, fmt.Errorf("open file: %w", err)
	}

	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat file: %w", err)
	}

	if !info.Mode().IsRegular() {
		return nil, errors.New("tail requires a regular file")
	}

	output, truncated, err := tailFromFile(file, info.Size(), lines, log)
	if err != nil {
		return nil, err
	}

	if truncated {
		output += "\n\n(tail truncated to fit the byte limit)"
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

// tailState accumulates the newest complete lines within the budgets.
type tailState struct {
	collected [][]byte
	total     int64
	full      bool
}

func (t *tailState) accept(line []byte, lines int) bool {
	if len(t.collected) >= lines || t.total+int64(len(line)) > tailMaxBytes {
		t.full = true

		return false
	}

	t.collected = append(t.collected, line)
	t.total += int64(len(line))

	return true
}

// acceptChunk feeds every line of the (older-first) chunk to accept.
func (t *tailState) acceptChunk(chunk []byte, lines int) {
	for line := range bytes.SplitSeq(chunk, []byte{'\n'}) {
		if len(line) == 0 && (t.total > 0 || len(t.collected) > 0) {
			continue
		}

		if !t.accept(line, lines) {
			return
		}
	}
}

// tailFromFile reads backward in blocks and assembles the bounded final lines.
func tailFromFile(file *os.File, size int64, lines int, log *zap.Logger) (string, bool, error) {
	if size == 0 {
		return "(no output)", false, nil
	}

	if err := rejectBinary(file, size); err != nil {
		return "", false, err
	}

	state := &tailState{}
	var carry []byte

	offset := size

	for offset > 0 && !state.full && len(state.collected) <= lines {
		block := min(int64(tailBlockReadSize), offset)
		offset -= block

		buf := make([]byte, block+int64(len(carry)))
		if _, err := file.ReadAt(buf[:block], offset); err != nil && !errors.Is(err, io.EOF) {
			return "", false, fmt.Errorf("read file tail: %w", err)
		}

		copy(buf[block:], carry)
		chunk := buf

		// A leading fragment without a preceding newline belongs to the next
		// (older) block unless this is the file start, where it is a line.
		lead := bytes.LastIndexByte(chunk, '\n')
		if lead >= 0 {
			lead++
		}

		if lead == 0 && offset != 0 {
			carry = chunk

			continue
		}

		if lead < len(chunk) && offset != 0 {
			carry = chunk[lead:]
			chunk = chunk[:lead]
		} else {
			carry = nil
		}

		state.acceptChunk(chunk, lines)
	}

	// Newest-first collection reverses for output order.
	slices.Reverse(state.collected)

	var out strings.Builder

	for i, line := range state.collected {
		if i > 0 {
			out.WriteByte('\n')
		}

		out.Write(truncateTailLine(line, log))
	}

	return out.String(), state.full, nil
}

// truncateTailLine applies read's per-line cap to one extracted line.
func truncateTailLine(line []byte, log *zap.Logger) []byte {
	const maxLen = maxLineLength

	_ = log

	return line[:min(len(line), maxLen)]
}

// rejectBinary sniffs the head of the file for binary content.
func rejectBinary(file *os.File, size int64) error {
	head := min(int64(4*1024), size)

	buf := make([]byte, head)
	if _, err := file.ReadAt(buf, 0); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("sniff file: %w", err)
	}

	nonPrintable := 0

	for _, b := range buf {
		if b == 0 {
			return errors.New("tail requires a text file")
		}

		if b < 32 && b != '\n' && b != '\r' && b != '\t' {
			nonPrintable++
		}
	}

	if len(buf) > 0 && nonPrintable*100 > len(buf)*30 {
		return errors.New("tail requires a text file")
	}

	return nil
}

var _ = filepath.Join
