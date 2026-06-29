package builtin

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// editRange represents the final position of an edit in the post-edit file.
type editRange struct {
	finalStart int // 1-based, inclusive
	finalEnd   int // 1-based, inclusive; finalEnd < finalStart means deletion
}

func executeStrReplace(content, oldString, newString string) (string, []editRange, error) {
	if oldString == newString {
		return "", nil, errors.New("oldString and newString must be different")
	}

	count := strings.Count(content, oldString)
	switch count {
	case 0:
		return "", nil, errors.New("oldString not found in file")
	case 1:
		// OK — unique match
	default:
		return "", nil, fmt.Errorf(
			"oldString found %d times in file — add more surrounding context to make it unique",
			count,
		)
	}

	before, after, _ := strings.Cut(content, oldString)
	newContent := before + newString + after

	// Compute line range of the replacement in the new content.
	lineNum := 1 + strings.Count(before, "\n")
	newLines := strings.Count(newString, "\n")
	endLine := lineNum + newLines

	ranges := []editRange{{
		finalStart: lineNum,
		finalEnd:   endLine,
	}}

	return newContent, ranges, nil
}

func executeReplaceAll(content, oldString, newString string) (string, []editRange, error) {
	if oldString == "" {
		return "", nil, errors.New("oldString is required")
	}

	if oldString == newString {
		return "", nil, errors.New("oldString and newString must be different")
	}

	if !strings.Contains(content, oldString) {
		return "", nil, errors.New("oldString not found in file")
	}

	// Pass 1: find all occurrence byte offsets in the original content.
	var offsets []int
	searchLen := len(oldString)
	start := 0

	for {
		idx := strings.Index(content[start:], oldString)
		if idx < 0 {
			break
		}

		offsets = append(offsets, start+idx)
		start += idx + searchLen
	}

	// Pass 2: produce new content and compute final line positions.
	newContent := strings.ReplaceAll(content, oldString, newString)
	delta := len(newString) - searchLen
	replaceNewlines := strings.Count(newString, "\n")

	ranges := make([]editRange, len(offsets))

	for i, origOffset := range offsets {
		adjustedOffset := origOffset + i*delta
		lineNum := 1 + strings.Count(newContent[:adjustedOffset], "\n")
		ranges[i] = editRange{
			finalStart: lineNum,
			finalEnd:   lineNum + replaceNewlines,
		}
	}

	return newContent, ranges, nil
}

// formatContextPreview formats context around each edited region.
func formatContextPreview(lines []string, regions []editRange) string {
	type window struct {
		start, end int // 1-based, inclusive
	}

	totalLines := len(lines)

	windows := make([]window, 0, len(regions))

	for _, r := range regions {
		var w window
		if r.finalEnd < r.finalStart {
			w.start = r.finalStart - 5
			w.end = r.finalStart + 4
		} else {
			w.start = r.finalStart - 5
			w.end = r.finalEnd + 5
		}

		if w.start < 1 {
			w.start = 1
		}

		if w.end > totalLines {
			w.end = totalLines
		}

		windows = append(windows, w)
	}

	sort.Slice(windows, func(i, j int) bool {
		return windows[i].start < windows[j].start
	})

	// Merge overlapping/adjacent windows (within 10 lines of each other).
	merged := []window{windows[0]}
	for i := 1; i < len(windows); i++ {
		last := &merged[len(merged)-1]
		if windows[i].start <= last.end+10 {
			if windows[i].end > last.end {
				last.end = windows[i].end
			}
		} else {
			merged = append(merged, windows[i])
		}
	}

	var sb strings.Builder
	for _, w := range merged {
		fmt.Fprintf(&sb, "\nResult around edit (lines %d-%d):\n", w.start, w.end)

		for lineNum := w.start; lineNum <= w.end; lineNum++ {
			fmt.Fprintf(&sb, "%d| %s\n", lineNum, lines[lineNum-1])
		}
	}

	return sb.String()
}

// tieredReplaceAllPreview formats context preview for replaceAll with tiering.
func tieredReplaceAllPreview(lines []string, ranges []editRange) string {
	n := len(ranges)

	switch {
	case n >= 10:
		return fmt.Sprintf("Replaced %d occurrences.\n", n) + formatContextPreview(lines, ranges[:1])
	case n >= 4:
		remaining := n - 3
		noun := "replacements"

		if remaining == 1 {
			noun = "replacement"
		}

		return formatContextPreview(lines, ranges[:3]) + fmt.Sprintf("\n... and %d more %s", remaining, noun)
	default:
		return formatContextPreview(lines, ranges)
	}
}
