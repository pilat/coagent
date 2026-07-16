package session

import (
	"fmt"
	"time"
)

// timestamper prefixes user messages with a compact timestamp and elapsed-since-last indicator.
// Zero value is ready to use (first message will have no elapsed prefix).
type timestamper struct {
	lastActivity time.Time
	nowFunc      func() time.Time // defaults to time.Now if nil
}

// now returns the current time, using nowFunc if set.
func (t *timestamper) now() time.Time {
	if t.nowFunc != nil {
		return t.nowFunc()
	}

	return time.Now()
}

// touch records activity (model response, tool result) without stamping a message.
// This keeps elapsed accurate: it measures the gap since any activity, not just user messages.
func (t *timestamper) touch() {
	t.lastActivity = t.now()
}

// stamp prefixes msg with "[+elapsed DOW YYYY-MM-DD HH:MM]".
// Empty messages pass through unchanged without advancing the clock.
func (t *timestamper) stamp(msg string) string {
	return t.stampAt(msg, t.now())
}

// stampAt stamps a durable message at receipt time. It never moves activity
// backwards if the session observed newer model/tool activity first.
func (t *timestamper) stampAt(msg string, now time.Time) string {
	if msg == "" {
		return ""
	}

	var prefix string

	if t.lastActivity.IsZero() {
		// First message in session — no elapsed.
		prefix = fmt.Sprintf("[%s]", now.Format("Mon 2006-01-02 15:04"))
	} else {
		elapsed := max(now.Sub(t.lastActivity), 0)

		prefix = fmt.Sprintf("[%s %s]", formatElapsed(elapsed), now.Format("Mon 2006-01-02 15:04"))
	}

	if now.After(t.lastActivity) {
		t.lastActivity = now
	}

	return prefix + " " + msg
}

// localTimezone returns the local timezone name. Falls back to the abbreviation
// (e.g., "CET", "UTC") when Location().String() returns "Local" (common in containers).
func localTimezone() string {
	now := time.Now()

	loc := now.Location().String()
	if loc != "Local" {
		return loc
	}

	abbrev, _ := now.Zone()

	return abbrev
}

// formatElapsed formats a duration as a compound human-readable string:
//   - <60s:  "+Xs"
//   - <1h:   "+XmYs"
//   - <24h:  "+XhYm"
//   - ≥24h:  "+XdYhZm"
//
// Zero components are omitted (e.g., "+5m" not "+5m0s"), except the leading unit
// which is always shown (e.g., "+0s" for zero duration).
func formatElapsed(d time.Duration) string {
	totalSeconds := int(d / time.Second)

	days := totalSeconds / 86400
	hours := (totalSeconds % 86400) / 3600
	minutes := (totalSeconds % 3600) / 60
	seconds := totalSeconds % 60

	switch {
	case days > 0:
		s := fmt.Sprintf("+%dd", days)
		if hours > 0 {
			s += fmt.Sprintf("%dh", hours)
		}

		if minutes > 0 {
			s += fmt.Sprintf("%dm", minutes)
		}

		return s
	case hours > 0:
		s := fmt.Sprintf("+%dh", hours)
		if minutes > 0 {
			s += fmt.Sprintf("%dm", minutes)
		}

		return s
	case minutes > 0:
		s := fmt.Sprintf("+%dm", minutes)
		if seconds > 0 {
			s += fmt.Sprintf("%ds", seconds)
		}

		return s
	default:
		return fmt.Sprintf("+%ds", seconds)
	}
}
