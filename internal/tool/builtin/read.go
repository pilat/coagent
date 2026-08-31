package builtin

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/tool"
)

const (
	defaultReadLimit = 2000
	maxLineLength    = 2000
	maxBytes         = 50 * 1024
	readDescription  = `Reads a file from the filesystem. You can access any file directly using this tool.

Usage:
- The file_path parameter can be absolute or relative to the working directory
- By default, it reads up to 2000 lines starting from the beginning of the file
- You can optionally specify a line offset and limit for pagination (especially handy for long files)
- Any lines longer than 2000 characters will be truncated
- Each line is returned as "lineNum| content" (e.g., "42| some code here")
- Image files (jpeg, png, gif, webp, up to 3.75 MB) are returned as viewable image attachments; text-only tools cannot see them otherwise
- Other binary files cannot be read

CRITICAL: You have the capability to call multiple tools in a single response. It is always better to speculatively read multiple files in parallel that are potentially useful.`
)

var _ tool.Tool = (*readTool)(nil)

type readParams struct {
	FilePath string `json:"file_path"`
	Offset   int    `json:"offset,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

type readTool struct {
	workDir string
}

func newReadTool(workDir string) *readTool {
	return &readTool{workDir: workDir}
}

func (t *readTool) ID() string          { return "read" }
func (t *readTool) Description() string { return readDescription }

func (t *readTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"file_path": {
				"type": "string",
				"description": "The path to the file to read"
			},
			"offset": {
				"type": "integer",
				"description": "The line number to start reading from (0-based)"
			},
			"limit": {
				"type": "integer",
				"description": "The number of lines to read (defaults to 2000)"
			}
		},
		"required": ["file_path"]
	}`)
}

func (t *readTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	log := logger.Ctx(ctx).Named("tool.read")

	p, err := t.parseReadParams(params, log)
	if err != nil {
		return nil, err
	}

	filePath := resolvePath(t.workDir, p.FilePath)

	// Supported images route to the pixel branch ahead of binary rejection;
	// offset/limit do not apply there. Stat first without opening: a FIFO must
	// never reach an open() call.
	if info, statErr := os.Stat(filePath); statErr == nil && info.Mode().IsRegular() {
		if mime := sniffImageMIME(filePath); mime != "" {
			return t.readImage(ctx, filePath, mime)
		}
	}

	offset, limit := normalizeReadBounds(p.Offset, p.Limit)

	lines, totalLines, truncatedByBytes, err := t.readLocked(filePath, offset, limit, log)
	if err != nil {
		return nil, err
	}

	output := formatReadOutput(lines, offset, totalLines, truncatedByBytes)
	title := relativeTitle(t.workDir, filePath)

	lastReadLine := offset + len(lines)
	truncated := totalLines > lastReadLine || truncatedByBytes

	log.Debug("complete",
		zap.String("filePath", filePath),
		zap.Int("linesRead", len(lines)),
		zap.Int("totalLines", totalLines),
		zap.Bool("truncated", truncated),
		zap.Int("outputSize", len(output)))

	return &tool.Result{
		Title:  title,
		Output: output,
		Metadata: map[string]any{
			"lines":          len(lines),
			"total":          totalLines,
			metaKeyTruncated: truncated,
		},
	}, nil
}

// readLocked validates and scans the file under the path's read lock: a
// mutation in the same batch rewrites the whole file, and an unsynchronized
// scan can observe it half-written.
func (t *readTool) readLocked(
	filePath string,
	offset, limit int,
	log *zap.Logger,
) ([]string, int, bool, error) {
	unlock := lockFileRead(filePath)
	defer unlock()

	if err := t.validateFile(filePath, log); err != nil {
		return nil, 0, false, err
	}

	return t.scanFile(filePath, offset, limit)
}

func (t *readTool) parseReadParams(params json.RawMessage, log *zap.Logger) (readParams, error) {
	var p readParams
	if err := json.Unmarshal(params, &p); err != nil {
		log.Warn("invalid_parameters", zap.Error(err))
		return p, fmt.Errorf("invalid parameters: %w", err)
	}

	if p.FilePath == "" {
		log.Warn("empty_filepath")
		return p, errors.New("file_path is required")
	}

	log.Debug("executing", zap.String("filePath", p.FilePath), zap.Int("offset", p.Offset), zap.Int("limit", p.Limit))

	return p, nil
}

func normalizeReadBounds(offset, limit int) (int, int) {
	if offset < 0 {
		offset = 0
	}

	if limit <= 0 {
		limit = defaultReadLimit
	}

	return offset, limit
}

func relativeTitle(workDir, filePath string) string {
	if workDir == "" {
		return filePath
	}

	if rel, err := filepath.Rel(workDir, filePath); err == nil {
		return rel
	}

	return filePath
}

func (t *readTool) validateFile(filePath string, log *zap.Logger) error {
	info, err := os.Stat(filePath)
	if os.IsNotExist(err) {
		log.Warn("file_not_found", zap.String("filePath", filePath))
		return fmt.Errorf("file not found: %s", filePath)
	}

	if err != nil {
		log.Warn("stat_failed", zap.String("filePath", filePath), zap.Error(err))
		return fmt.Errorf("stat file: %w", err)
	}

	if info.IsDir() {
		log.Warn("path_is_directory", zap.String("filePath", filePath))
		return fmt.Errorf("path is a directory, not a file: %s", filePath)
	}

	// A FIFO/device/socket has no writer; os.Open blocks in the kernel,
	// uncancelable by ctx. stat (already done) never blocks.
	if !info.Mode().IsRegular() {
		log.Warn("non_regular_rejected", zap.String("filePath", filePath))
		return fmt.Errorf("not a regular file: %s", filePath)
	}

	if isBinaryFile(filePath) {
		log.Warn("binary_rejected", zap.String("filePath", filePath))
		return fmt.Errorf("cannot read binary file: %s", filePath)
	}

	log.Debug("file_opened", zap.String("filePath", filePath), zap.Int64("size", info.Size()))

	return nil
}

func (t *readTool) scanFile(
	filePath string,
	offset, limit int,
) ([]string, int, bool, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return nil, 0, false, fmt.Errorf("open file: %w", err)
	}

	defer func() { _ = file.Close() }()

	var bytesRead int
	var lines []string
	var totalLines int
	var truncatedByBytes bool

	scanner := bufio.NewScanner(file)
	lineNum := 0

	for scanner.Scan() {
		totalLines++

		if lineNum < offset {
			lineNum++
			continue
		}

		if len(lines) >= limit {
			lineNum++
			continue
		}

		line := scanner.Text()
		if len(line) > maxLineLength {
			line = line[:maxLineLength] + "..."
		}

		lineBytes := len(line) + 1 // +1 for newline
		if bytesRead+lineBytes > maxBytes {
			truncatedByBytes = true
			break
		}

		lines = append(lines, line)
		bytesRead += lineBytes
		lineNum++
	}

	if err := scanner.Err(); err != nil {
		return nil, 0, false, fmt.Errorf("error reading file: %w", err)
	}

	if truncatedByBytes {
		for scanner.Scan() {
			totalLines++
		}
	}

	return lines, totalLines, truncatedByBytes, nil
}

func formatReadOutput(lines []string, offset, totalLines int, truncatedByBytes bool) string {
	var output strings.Builder
	output.WriteString("<file>\n")

	for i, line := range lines {
		lineNumber := offset + i + 1
		fmt.Fprintf(&output, "%d| %s\n", lineNumber, line)
	}

	lastReadLine := offset + len(lines)
	hasMoreLines := totalLines > lastReadLine

	switch {
	case truncatedByBytes:
		fmt.Fprintf(&output, "\n(Output truncated at %d bytes. Use 'offset' parameter to read beyond line %d)",
			maxBytes, lastReadLine)
	case hasMoreLines:
		fmt.Fprintf(&output, "\n(File has more lines. Use 'offset' parameter to read beyond line %d)", lastReadLine)
	default:
		fmt.Fprintf(&output, "\n(End of file - total %d lines)", totalLines)
	}

	output.WriteString("\n</file>")

	return output.String()
}

// isBinaryFile checks if a file is binary by examining its contents.
func isBinaryFile(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	binaryExts := map[string]bool{
		".zip": true, ".tar": true, ".gz": true, ".exe": true,
		".dll": true, ".so": true, ".class": true, ".jar": true,
		".war": true, ".7z": true, ".doc": true, ".docx": true,
		".xls": true, ".xlsx": true, ".ppt": true, ".pptx": true,
		".odt": true, ".ods": true, ".odp": true, ".bin": true,
		".dat": true, ".obj": true, ".o": true, ".a": true,
		".lib": true, ".wasm": true, ".pyc": true, ".pyo": true,
		".png": true, ".jpg": true, ".jpeg": true, ".gif": true,
		".bmp": true, ".ico": true, ".webp": true, ".mp3": true,
		".mp4": true, ".avi": true, ".mov": true, ".wav": true,
		".pdf": true,
	}

	if binaryExts[ext] {
		return true
	}

	// Check first 4KB for null bytes or high ratio of non-printable chars
	file, err := os.Open(path)
	if err != nil {
		return false
	}

	defer func() { _ = file.Close() }()

	buf := make([]byte, 4096)

	n, err := file.Read(buf)
	if err != nil || n == 0 {
		return false
	}

	buf = buf[:n]

	nonPrintable := 0

	for _, b := range buf {
		if b == 0 {
			return true
		}

		if b < 9 || (b > 13 && b < 32) {
			nonPrintable++
		}
	}

	return float64(nonPrintable)/float64(n) > 0.3
}
