package tool

import (
	"fmt"
	"time"
)

// SleepParams is the sleep tool's wire contract. It stays here (not in schedule
// with the tool) so the session's resume path can re-parse the args too.
type SleepParams struct {
	Duration string `json:"duration"`
	Reason   string `json:"reason,omitempty"`
}

// ParseDuration tries Go duration, human-friendly units (d/w), then RFC3339.
func ParseDuration(s string) (time.Duration, error) {
	if dur, err := time.ParseDuration(s); err == nil {
		return dur, nil
	}

	// Human-friendly: "1d", "3d", "1w", "2w"
	if dur, ok := parseHumanDuration(s); ok {
		return dur, nil
	}

	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return time.Until(t), nil
	}

	return 0, fmt.Errorf("invalid duration %q: not a Go duration or RFC3339 timestamp", s)
}

// parseHumanDuration handles "Nd" (days) and "Nw" (weeks) that Go stdlib doesn't support.
func parseHumanDuration(s string) (time.Duration, bool) {
	if len(s) < 2 {
		return 0, false
	}

	suffix := s[len(s)-1]
	numStr := s[:len(s)-1]

	var multiplier time.Duration

	switch suffix {
	case 'd':
		multiplier = 24 * time.Hour
	case 'w':
		multiplier = 7 * 24 * time.Hour
	default:
		return 0, false
	}

	var n int

	for _, c := range numStr {
		if c < '0' || c > '9' {
			return 0, false
		}

		n = n*10 + int(c-'0')
	}

	if n == 0 {
		return 0, false
	}

	return time.Duration(n) * multiplier, true
}
