package telegram

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

const preMaxWidth = 58

var (
	reTable    = regexp.MustCompile(`(?m)^\|.+\|(?:\n\|.+\|)+`)
	reTableSep = regexp.MustCompile(`^\|[\s\-:|]+\|$`)
)

// renderMarkdownTable adapts a markdown table to Telegram, which has no table tag:
// narrow tables become an aligned <pre>, wider ones collapse to key/value or card text.
func renderMarkdownTable(raw string) string {
	headers, rows := parseMarkdownTable(raw)
	if len(headers) == 0 {
		return "<pre>" + escapeHTML(raw) + "</pre>"
	}

	colWidths := make([]int, len(headers))
	for i, h := range headers {
		w := displayWidth(h)

		for _, r := range rows {
			if i < len(r) {
				w = max(w, displayWidth(r[i]))
			}
		}

		colWidths[i] = w
	}

	total := len(headers)*3 + 1
	for _, w := range colWidths {
		total += w
	}

	switch {
	case total <= preMaxWidth:
		return renderNarrowTable(headers, rows, colWidths)
	case len(headers) == 2:
		return renderTwoColTable(rows)
	default:
		return renderCardTable(headers, rows)
	}
}

func parseMarkdownTable(raw string) ([]string, [][]string) {
	var (
		headers []string
		rows    [][]string
	)

	for line := range strings.SplitSeq(raw, "\n") {
		if reTableSep.MatchString(line) {
			continue
		}

		line = reBold.ReplaceAllString(line, "$1")
		line = strings.TrimSuffix(strings.TrimPrefix(line, "|"), "|")
		parts := strings.Split(line, "|")

		row := make([]string, len(parts))
		for i, p := range parts {
			row[i] = strings.TrimSpace(p)
		}

		if headers == nil {
			headers = row
			continue
		}

		rows = append(rows, row)
	}

	return headers, rows
}

// displayWidth counts runes, not bytes, so column padding stays aligned for Cyrillic.
func displayWidth(s string) int {
	return utf8.RuneCountInString(s)
}

// cellToHTML escapes the cell, then wraps `inline code` — safe because escaping never emits a backtick.
func cellToHTML(cell string) string {
	escaped := escapeHTML(cell)
	return reInlineCode.ReplaceAllString(escaped, "<code>$1</code>")
}

func renderNarrowTable(headers []string, rows [][]string, colWidths []int) string {
	pad := func(s string, w int) string {
		return s + strings.Repeat(" ", w-displayWidth(s))
	}

	lines := make([]string, 0, len(rows)+1)

	head := make([]string, len(headers))
	for i, h := range headers {
		head[i] = pad(h, colWidths[i])
	}

	lines = append(lines, "| "+strings.Join(head, " | ")+" |")

	for _, r := range rows {
		cells := make([]string, len(colWidths))
		for i := range colWidths {
			cell := ""
			if i < len(r) {
				cell = r[i]
			}

			cells[i] = pad(cell, colWidths[i])
		}

		lines = append(lines, "| "+strings.Join(cells, " | ")+" |")
	}

	return "<pre>" + escapeHTML(strings.Join(lines, "\n")) + "</pre>"
}

func renderTwoColTable(rows [][]string) string {
	lines := make([]string, 0, len(rows))
	for _, r := range rows {
		key, val := "", ""

		if len(r) > 0 {
			key = r[0]
		}

		if len(r) > 1 {
			val = r[1]
		}

		lines = append(lines, "<b>"+cellToHTML(key)+"</b>: "+cellToHTML(val))
	}

	return strings.Join(lines, "\n")
}

func renderCardTable(headers []string, rows [][]string) string {
	cards := make([]string, 0, len(rows))
	for _, r := range rows {
		title := ""
		if len(r) > 0 {
			title = r[0]
		}

		fields := make([]string, 0, len(headers))
		for i := 1; i < len(headers); i++ {
			val := ""
			if i < len(r) {
				val = r[i]
			}

			fields = append(fields, "<b>"+escapeHTML(headers[i])+"</b>: "+cellToHTML(val))
		}

		card := "━━ " + cellToHTML(title) + " ━━"
		if len(fields) > 0 {
			card += "\n" + strings.Join(fields, "\n")
		}

		cards = append(cards, card)
	}

	return strings.Join(cards, "\n\n")
}
