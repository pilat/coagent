package backgroundprocess

import (
	"bytes"
	"os"
)

// tailBlockSize is the reverse-scan block size.
const tailBlockSize = 8 * 1024

// maxTailBinaryBlock bounds the binary sniff for tail extraction.
const maxTailBinaryBlock = 4 * 1024

// ExtractTail returns the final lines of a regular text file without scanning
// it from byte zero. ok=false marks unreadable, non-regular, or binary files.
func ExtractTail(path string, maxLines int, maxBytes int64) (string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}

	defer file.Close()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}

	if isBinaryFile(file) {
		return "", false
	}

	if info.Size() == 0 {
		return "", true
	}

	lines := scanTailLines(file, info.Size(), maxLines, maxBytes)

	return joinTailLines(lines), true
}

// tailState carries the accumulation across backward blocks.
type tailState struct {
	lines [][]byte
	total int64
	full  bool
}

// accept prepends one older line if budgets allow; it reports saturation.
func (t *tailState) accept(line []byte, maxLines int, maxBytes int64) bool {
	if len(t.lines) >= maxLines || t.total+int64(len(line)) > maxBytes {
		t.full = true

		return false
	}

	t.lines = append([][]byte{line}, t.lines...)
	t.total += int64(len(line))

	return true
}

// scanTailLines reads blocks backward, keeping the newest complete lines
// within the line and byte budgets.
func scanTailLines(file *os.File, size int64, maxLines int, maxBytes int64) [][]byte {
	state := &tailState{}
	var carry []byte

	offset := size

	for offset > 0 && !state.full && len(state.lines) <= maxLines {
		block := min(int64(tailBlockSize), offset)
		offset -= block

		buf := make([]byte, block+int64(len(carry)))
		if _, err := file.ReadAt(buf[:block], offset); err != nil {
			break
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

		start := 0

		for i, b := range chunk {
			if b != '\n' {
				continue
			}

			line := chunk[start:i]
			start = i + 1

			if !state.accept(line, maxLines, maxBytes) {
				return state.lines
			}
		}

		if start < len(chunk) {
			if !state.accept(chunk[start:], maxLines, maxBytes) {
				return state.lines
			}
		}
	}

	return state.lines
}

func joinTailLines(lines [][]byte) string {
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

// truncateTailBytes shrinks the extracted text to maxBytes on a UTF-8 boundary.
func truncateTailBytes(text string, maxBytes int64) (string, bool) {
	if int64(len(text)) <= maxBytes {
		return text, false
	}

	limit := int(maxBytes)

	for limit > 0 && limit < len(text) && text[limit]&0xC0 == 0x80 {
		limit--
	}

	return text[:limit], true
}
