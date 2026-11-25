package session

import "strings"

const (
	hookFeedbackLimit      = 10000
	hookFeedbackTruncation = "\n[hook feedback truncated]"
)

func appendHookFeedback(content string, feedback []string) string {
	if len(feedback) == 0 {
		return content
	}

	seen := make(map[string]struct{}, len(feedback))
	var unique []string

	for _, entry := range feedback {
		if entry == "" {
			continue
		}

		if _, exists := seen[entry]; exists {
			continue
		}

		seen[entry] = struct{}{}
		unique = append(unique, entry)
	}

	if len(unique) == 0 {
		return content
	}

	joined := []rune(strings.Join(unique, "\n\n"))
	if len(joined) > hookFeedbackLimit {
		joined = append(joined[:hookFeedbackLimit-len(hookFeedbackTruncation)], []rune(hookFeedbackTruncation)...)
	}

	return content + "\n\n" + string(joined)
}
