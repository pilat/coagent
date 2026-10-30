package controllerapi

import (
	"strings"
	"unicode/utf8"
)

const (
	SessionAttributeTitle = "title"
	SessionTitleChars     = 80
)

// NormalizeSessionTitle trims a one-line title and enforces its rune limit.
func NormalizeSessionTitle(raw string) (string, bool) {
	title := strings.TrimSpace(raw)

	return title, title != "" && !strings.ContainsAny(title, "\r\n") &&
		utf8.RuneCountInString(title) <= SessionTitleChars
}

// SessionTitleFromInput names work from the first line of non-command input.
func SessionTitleFromInput(content string) string {
	title := strings.TrimSpace(content)
	if title == "" || strings.HasPrefix(title, "/") {
		return ""
	}

	if end := strings.IndexAny(title, "\r\n"); end >= 0 {
		title = strings.TrimSpace(title[:end])
	}

	runes := []rune(title)
	if len(runes) > SessionTitleChars {
		return string(runes[:SessionTitleChars-1]) + "…"
	}

	return title
}
