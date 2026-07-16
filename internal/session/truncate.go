package session

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// truncateHeadTail truncates s to maxRunes using a head+tail strategy,
// splitting 70% head / 30% tail by default. If the tail contains
// error/stack-trace patterns, the split is adjusted to 50/50 to preserve
// more tail context.
// Cut points are snapped to the nearest newline boundary when possible.
// A middle marker showing omitted size is counted inside the limit.
// Returns s unchanged if under maxRunes.
func truncateHeadTail(s string, maxRunes int) string {
	const defaultHeadRatio = 0.7

	headRatio := defaultHeadRatio

	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}

	marker := fmt.Sprintf("\n... (omitted %d chars) ...\n", len(runes)-maxRunes)
	markerRunes := []rune(marker)

	available := maxRunes - len(markerRunes)
	if available <= 0 {
		// Degenerate case: marker alone exceeds limit
		return string(runes[:maxRunes])
	}

	// Adjust ratio if tail contains error/stack-trace content
	if available > 200 && hasImportantTail(s) {
		headRatio = 0.5
	}

	headSize := int(float64(available) * headRatio)
	tailSize := available - headSize

	// Snap to newline boundaries for readability
	maxDrift := min(200, available/5)
	headSize = snapToNewline(runes, headSize, -1, maxDrift)
	tailStart := len(runes) - tailSize
	tailStart = snapToNewline(runes, tailStart, +1, maxDrift)

	// Guard: ensure tailStart stays within bounds and tail is non-empty
	if tailStart >= len(runes) {
		tailStart = len(runes) - tailSize // revert to exact
	}

	tailSize = len(runes) - tailStart

	// Ensure we don't exceed budget after snapping
	if headSize+tailSize > available {
		// Revert to exact cuts
		headSize = int(float64(available) * headRatio)
		tailSize = available - headSize
		tailStart = len(runes) - tailSize
	}

	head := runes[:headSize]
	tail := runes[tailStart:]

	return string(head) + marker + string(tail)
}

// hasImportantTail checks whether the tail of a string contains error or
// diagnostic content worth preserving. Inspects the last ~2000 runes.
// Uses specific multi-word patterns to avoid false positives on normal output.
func hasImportantTail(s string) bool {
	tailSample := s

	if utf8.RuneCountInString(s) > 2000 {
		runes := []rune(s)
		tailSample = string(runes[len(runes)-2000:])
	}

	lower := strings.ToLower(tailSample)

	// Error/diagnostic patterns — multi-word to reduce false positives
	for _, pattern := range []string{
		"error:", "exception:", "fatal:", "fatal error",
		"traceback (most recent", "panic:", "stack trace",
		"errno", "exit code", "exit status",
		"--- fail:", "build failed",
	} {
		if strings.Contains(lower, pattern) {
			return true
		}
	}

	// JSON closing brace/bracket at end of content
	trimmed := strings.TrimRight(tailSample, " \t\n\r")
	if strings.HasSuffix(trimmed, "}") || strings.HasSuffix(trimmed, "]") {
		return true
	}

	return false
}

// snapToNewline adjusts pos to the nearest newline boundary.
// direction -1 searches backward (for head cuts), +1 searches forward (for tail starts).
// maxDrift limits how far to search. Returns original pos if no newline found within range.
func snapToNewline(runes []rune, pos, direction, maxDrift int) int {
	if pos <= 0 || pos >= len(runes) || maxDrift <= 0 {
		return pos
	}

	if direction == -1 {
		// Search backward for '\n', return the position after it (start of next line)
		for i := pos; i >= pos-maxDrift && i >= 0; i-- {
			if runes[i] == '\n' {
				return i + 1
			}
		}

		return pos
	}

	// Search forward for '\n', return the position after it
	limit := min(pos+maxDrift, len(runes)-1)
	for i := pos; i <= limit; i++ {
		if runes[i] == '\n' {
			return i + 1
		}
	}

	return pos
}
