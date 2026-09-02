package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/tool"
)

// sleepTool creates a one-shot schedule and suspends the session.
// The scheduler resumes the session when the timer fires.
type sleepTool struct {
	svc       Service
	sessionID int64
}

var _ tool.Tool = (*sleepTool)(nil)

// NewSleepTool builds the "sleep" tool bound to a session and schedule service.
func NewSleepTool(svc Service, sessionID int64) tool.Tool {
	return &sleepTool{svc: svc, sessionID: sessionID}
}

func (t *sleepTool) ID() string          { return tool.IDSleep }
func (t *sleepTool) ParallelSafe() bool  { return false }
func (t *sleepTool) Description() string { return sleepDescription }

func (t *sleepTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"duration": {
				"type": "string",
				"description": "How long to sleep. Examples: \"30s\", \"5m\", \"2h\", \"3d\", \"1w\", \"2026-03-25T09:00:00Z\""
			},
			"reason": {
				"type": "string",
				"description": "Optional: why the agent is sleeping (e.g. waiting for CI, rate limit cooldown)"
			}
		},
		"required": ["duration"]
	}`)
}

func (t *sleepTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	if t.svc == nil {
		return nil, errors.New("sleep is not available in this context (no daemon)")
	}

	var p tool.SleepParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	if p.Duration == "" {
		return nil, errors.New("duration is required")
	}

	dur, err := tool.ParseDuration(p.Duration)
	if err != nil {
		return nil, fmt.Errorf("parse duration: %w", err)
	}

	if dur <= 0 {
		return nil, fmt.Errorf("duration must be positive, got %s", dur)
	}

	// Create a one-shot schedule and suspend the session.
	// The tool result is NOT recorded — the runner injects the real outcome
	// ("Sleep completed" or "Sleep interrupted") on resume.
	wakeAt := time.Now().Add(dur).UTC()

	callID := tool.CallIDFromContext(ctx)
	if callID == "" {
		return nil, errors.New("sleep requires a tool call id")
	}

	inputMsg := fmt.Sprintf(
		"Sleep completed (%s). Current time: %s. The wake-up schedule has been automatically removed.",
		p.Duration,
		wakeAt.Format(time.RFC3339),
	)

	if _, err := t.svc.AddSleep(ctx, t.sessionID, callID, wakeAt, inputMsg); err != nil {
		return nil, fmt.Errorf("store sleep schedule: %w", err)
	}

	return nil, tool.ErrSuspend
}

const sleepDescription = `Sleep (wait/pause/delay/nap) for a specified duration. This is a one-time timer — NOT for recurring schedules.

Examples: sleep("30s"), sleep("5m"), sleep("2h"), sleep("3d"), sleep("1w"), sleep("2026-03-25T09:00:00Z")

Use when you need to wait once before continuing: CI pipelines, deployments, rate limits, countdowns, user-requested delays, "wake me up at 9am", "remind me in 2 hours".
While sleeping, the agent uses zero tokens and zero compute.

Duration formats:
- Short: "30s", "5m", "2h", "2h30m"
- Days/weeks: "1d", "3d", "1w", "2w"
- Exact time: RFC3339 timestamp like "2026-03-25T09:00:00Z"

For recurring wake-ups (every hour, daily reminders, periodic checks), use the "schedule" tool with a cron expression instead.`
