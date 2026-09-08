package backgroundprocess

import (
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"unicode/utf8"
)

// tailBlockSize is the reverse-scan block size.
const tailBlockSize = 8 * 1024

// maxTailBinaryBlock bounds the binary sniff for tail extraction.
const maxTailBinaryBlock = 4 * 1024

// ScanTailLines returns the newest complete lines in file order. lineLimit
// truncates each line like read; zero preserves complete line bytes.
func ScanTailLines(
	file *os.File,
	size int64,
	maxLines int,
	maxBytes int64,
	lineLimit int,
) ([][]byte, bool, error) {
	if size == 0 {
		return nil, false, nil
	}

	if maxLines <= 0 || maxBytes <= 0 {
		return nil, true, nil
	}

	state := newTailScan(maxLines, maxBytes, lineLimit)
	position := size
	atEnd := true
	leadingDelimiter := false

	for position > 0 {
		block := min(int64(tailBlockSize), position)
		position -= block
		buf := make([]byte, block)

		if _, err := file.ReadAt(buf, position); err != nil && !errors.Is(err, io.EOF) {
			return nil, false, fmt.Errorf("read file tail: %w", err)
		}

		for i, value := range slices.Backward(buf) {
			if value != '\n' {
				state.line.add(value)

				atEnd = false

				continue
			}

			if atEnd {
				atEnd = false
				continue
			}

			if !state.acceptLine() {
				return state.result(), true, nil
			}

			leadingDelimiter = position == 0 && i == 0
		}
	}

	if state.line.total > 0 || leadingDelimiter {
		if !state.acceptLine() {
			return state.result(), true, nil
		}
	}

	return state.result(), false, nil
}

// ExtractTail returns the final lines of a regular text file without scanning
// it from byte zero. ok=false marks unreadable, non-regular, or binary files;
// truncated marks that older complete lines were dropped for a budget.
func ExtractTail(path string, maxLines int, maxBytes int64) (string, bool, bool) {
	// Stat before open: opening a FIFO for reading blocks until a writer
	// appears, so regularity is checked on metadata first.
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return "", false, false
	}

	file, err := os.Open(path)
	if err != nil {
		return "", false, false
	}

	defer file.Close()

	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", false, false
	}

	if isBinaryFile(file) {
		return "", false, false
	}

	if info.Size() == 0 {
		return "", false, true
	}

	lines, truncated, err := ScanTailLines(file, info.Size(), maxLines, maxBytes, 0)
	if err != nil {
		return "", false, false
	}

	return joinLines(lines), truncated, true
}

type tailScan struct {
	lines    [][]byte
	total    int64
	maxLines int
	maxBytes int64
	line     reverseLine
}

type reverseLine struct {
	buf   []byte
	total int64
	next  int
	limit int
}

func newTailScan(maxLines int, maxBytes int64, lineLimit int) *tailScan {
	captureLimit := lineLimit
	if captureLimit == 0 {
		captureLimit = int(maxBytes)
	}

	return &tailScan{
		maxLines: maxLines,
		maxBytes: maxBytes,
		line: reverseLine{
			buf:   make([]byte, captureLimit),
			limit: lineLimit,
		},
	}
}

func (t *tailScan) acceptLine() bool {
	line := t.line.render()

	additional := int64(len(line))
	if t.line.limit == 0 {
		additional = t.line.total
	}

	if len(t.lines) > 0 {
		additional++
	}

	if len(t.lines) >= t.maxLines || t.total+additional > t.maxBytes {
		return false
	}

	t.lines = append(t.lines, line)
	t.total += additional
	t.line.reset()

	return true
}

func (t *tailScan) result() [][]byte {
	slices.Reverse(t.lines)

	return t.lines
}

func (l *reverseLine) add(value byte) {
	if len(l.buf) > 0 {
		l.buf[l.next%len(l.buf)] = value
		l.next++
	}

	l.total++
}

func (l *reverseLine) render() []byte {
	size := min(l.total, int64(len(l.buf)))
	line := make([]byte, size)
	start := l.next - int(size)

	for i := range line {
		line[len(line)-1-i] = l.buf[(start+i)%len(l.buf)]
	}

	if l.limit > 0 && l.total > int64(l.limit) {
		for len(line) > 0 && !utf8.Valid(line) {
			line = line[:len(line)-1]
		}

		truncated := make([]byte, len(line)+3)
		copy(truncated, line)
		copy(truncated[len(line):], "...")
		line = truncated
	}

	return line
}

func (l *reverseLine) reset() {
	l.total = 0
	l.next = 0
}

func joinLines(lines [][]byte) string {
	out := make([]byte, 0, 1024)

	for i, line := range lines {
		if i > 0 {
			out = append(out, '\n')
		}

		out = append(out, line...)
	}

	return string(out)
}

func isBinaryFile(file *os.File) bool {
	buf := make([]byte, maxTailBinaryBlock)

	n, err := file.ReadAt(buf, 0)
	if err != nil && n == 0 {
		return false
	}

	buf = buf[:n]

	nonPrintable := 0

	for _, b := range buf {
		if b == 0 {
			return true
		}

		if b < 32 && b != '\n' && b != '\r' && b != '\t' {
			nonPrintable++
		}
	}

	return len(buf) > 0 && nonPrintable*100 > len(buf)*30
}

// ValidEventTail reports whether the extracted preview may enter a completion
// event: a non-UTF-8 tail is reported as an omitted binary preview.
func ValidEventTail(tail string) bool {
	return utf8.ValidString(tail)
}

// TruncateTailToBytes shrinks a whole-line tail to maxBytes, keeping whole
// lines and a UTF-8 boundary.
func TruncateTailToBytes(text string, maxBytes int64) string {
	if int64(len(text)) <= maxBytes {
		return text
	}

	start := len(text) - int(maxBytes)
	for start < len(text) && text[start]&0xC0 == 0x80 {
		start++
	}

	trimmed := text[start:]

	if start > 0 {
		idx := strings.IndexByte(trimmed, '\n')
		if idx < 0 {
			return ""
		}

		trimmed = trimmed[idx+1:]
	}

	return trimmed
}
