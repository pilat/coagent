package sessionprompt

import (
	"fmt"
	"time"
)

// Timestamper prefixes user messages with a timestamp and elapsed-since-last indicator.
// Zero value is ready to use (first message will have no elapsed prefix).
type Timestamper struct {
	lastActivity time.Time
}

// NewTimestamper restores the last observed activity time.
func NewTimestamper(lastActivity time.Time) Timestamper {
	return Timestamper{lastActivity: lastActivity}
}

// Touch records model or tool activity without stamping a message.
// This keeps elapsed accurate: it measures the gap since any activity, not just user messages.
func (t *Timestamper) Touch() {
	t.lastActivity = time.Now()
}

// Stamp prefixes msg with "[+elapsed DOW YYYY-MM-DD HH:MM ZONE ±HH:MM]".
// Empty messages pass through unchanged without advancing the clock.
func (t *Timestamper) Stamp(msg string) string {
	return t.StampAt(msg, time.Now())
}

// StampAt stamps a durable message at receipt time. It never moves activity
// backwards if the session observed newer model/tool activity first.
func (t *Timestamper) StampAt(msg string, now time.Time) string {
	if msg == "" {
		return ""
	}

	var prefix string

	if t.lastActivity.IsZero() {
		prefix = fmt.Sprintf("[%s]", now.Format("Mon 2006-01-02 15:04 MST -07:00"))
	} else {
		elapsed := max(now.Sub(t.lastActivity), 0)

		prefix = fmt.Sprintf("[%s %s]", formatElapsed(elapsed), now.Format("Mon 2006-01-02 15:04 MST -07:00"))
	}

	if now.After(t.lastActivity) {
		t.lastActivity = now
	}

	return prefix + " " + msg
}

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
