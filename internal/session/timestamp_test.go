package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestTimestampStamp(t *testing.T) {
	base := time.Date(2026, 3, 29, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		elapsed time.Duration
		msg     string
		want    string
	}{
		{
			name: "first message no elapsed",
			msg:  "hello",
			want: "[Sun 2026-03-29 10:00] hello",
		},
		{
			name:    "30 seconds shows seconds",
			elapsed: 30 * time.Second,
			msg:     "quick follow-up",
			want:    "[+30s Sun 2026-03-29 10:00] quick follow-up",
		},
		{
			name:    "exactly 60s shows 1m",
			elapsed: 60 * time.Second,
			msg:     "one minute later",
			want:    "[+1m Sun 2026-03-29 10:01] one minute later",
		},
		{
			name:    "90s shows 1m30s",
			elapsed: 90 * time.Second,
			msg:     "after 90 seconds",
			want:    "[+1m30s Sun 2026-03-29 10:01] after 90 seconds",
		},
		{
			name:    "2 hour gap",
			elapsed: 2 * time.Hour,
			msg:     "after 2 hours",
			want:    "[+2h Sun 2026-03-29 12:00] after 2 hours",
		},
		{
			name:    "2h30m compound",
			elapsed: 2*time.Hour + 30*time.Minute,
			msg:     "after 2.5 hours",
			want:    "[+2h30m Sun 2026-03-29 12:30] after 2.5 hours",
		},
		{
			name:    "1 day gap",
			elapsed: 24 * time.Hour,
			msg:     "next day",
			want:    "[+1d Mon 2026-03-30 10:00] next day",
		},
		{
			name:    "1d2h15m compound",
			elapsed: 26*time.Hour + 15*time.Minute,
			msg:     "over a day",
			want:    "[+1d2h15m Mon 2026-03-30 12:15] over a day",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			now := base
			ts := timestamper{
				nowFunc: func() time.Time { return now },
			}

			if tt.elapsed > 0 {
				// Send first message to set lastActivity.
				ts.stamp("setup")
				now = now.Add(tt.elapsed)
			}

			got := ts.stamp(tt.msg)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestTimestampEmptyPassthrough(t *testing.T) {
	base := time.Date(2026, 3, 29, 10, 0, 0, 0, time.UTC)
	now := base
	ts := timestamper{
		nowFunc: func() time.Time { return now },
	}

	// First real message sets the clock.
	got := ts.stamp("first")
	assert.Equal(t, "[Sun 2026-03-29 10:00] first", got)

	// Empty message does NOT advance the clock.
	now = now.Add(5 * time.Minute)
	got = ts.stamp("")
	assert.Empty(t, got)

	// Next real message shows elapsed from "first", not from the empty call.
	now = now.Add(5 * time.Minute)
	got = ts.stamp("second")
	assert.Equal(t, "[+10m Sun 2026-03-29 10:10] second", got)
}

func TestTimestampTouchResetsElapsed(t *testing.T) {
	base := time.Date(2026, 3, 29, 10, 0, 0, 0, time.UTC)
	now := base
	ts := timestamper{
		nowFunc: func() time.Time { return now },
	}

	// User sends message at t=0.
	got := ts.stamp("hello")
	assert.Equal(t, "[Sun 2026-03-29 10:00] hello", got)

	// Model responds at t=+5m (touch advances the clock).
	now = now.Add(5 * time.Minute)
	ts.touch()

	// User sends another message at t=+8m (3m after model response).
	// Elapsed should be +3m (from touch), not +8m (from last stamp).
	now = now.Add(3 * time.Minute)
	got = ts.stamp("follow-up")
	assert.Equal(t, "[+3m Sun 2026-03-29 10:08] follow-up", got)
}

func TestFormatElapsed(t *testing.T) {
	tests := []struct {
		name string
		d    time.Duration
		want string
	}{
		{"zero", 0, "+0s"},
		{"5 seconds", 5 * time.Second, "+5s"},
		{"59 seconds", 59 * time.Second, "+59s"},
		{"exactly 1 minute", 60 * time.Second, "+1m"},
		{"1m30s", 90 * time.Second, "+1m30s"},
		{"5m", 5 * time.Minute, "+5m"},
		{"59m59s", 59*time.Minute + 59*time.Second, "+59m59s"},
		{"exactly 1 hour", time.Hour, "+1h"},
		{"1h1m", time.Hour + time.Minute, "+1h1m"},
		{"2h30m", 2*time.Hour + 30*time.Minute, "+2h30m"},
		{"23h59m", 23*time.Hour + 59*time.Minute, "+23h59m"},
		{"exactly 1 day", 24 * time.Hour, "+1d"},
		{"1d2h", 26 * time.Hour, "+1d2h"},
		{"1d0h15m", 24*time.Hour + 15*time.Minute, "+1d15m"},
		{"2d", 48 * time.Hour, "+2d"},
		{"3d5m", 72*time.Hour + 5*time.Minute, "+3d5m"},
		{"3d2h15m", 74*time.Hour + 15*time.Minute, "+3d2h15m"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, formatElapsed(tt.d))
		})
	}
}
