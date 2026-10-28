package assistant

import "strings"

func withUserWords(text, words string) string {
	words = strings.ReplaceAll(strings.TrimSpace(words), "\r\n", "\n")
	if words == "" || strings.HasPrefix(strings.TrimSpace(text), "/") || strings.Contains(text, words) {
		return text
	}

	lines := strings.Split(words, "\n")
	for i, line := range lines {
		lines[i] = ">"
		if line != "" {
			lines[i] += " " + line
		}
	}

	return text + "\n\nThe user's own words (they work with several sessions at once, so part of this may concern other work):\n" + strings.Join(
		lines,
		"\n",
	)
}
