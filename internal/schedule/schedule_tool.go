package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/pilat/coagent/internal/tool"
)

type scheduleParams struct {
	Action string `json:"action"`
	Cron   string `json:"cron"`
	ID     int64  `json:"id"`
	Prompt string `json:"prompt"`
	Fresh  bool   `json:"fresh"`
}

type scheduleTool struct {
	sessionID int64
	svc       Service
	location  *time.Location
}

var _ tool.Tool = (*scheduleTool)(nil)

// NewScheduleTool builds the "schedule" tool bound to a session and schedule service.
func NewScheduleTool(sessionID int64, svc Service, loc *time.Location) tool.Tool {
	if loc == nil {
		loc = time.UTC
	}

	return &scheduleTool{sessionID: sessionID, svc: svc, location: loc}
}

func (t *scheduleTool) ID() string { return tool.IDSchedule }

func (t *scheduleTool) Description() string {
	tzName := t.location.String()
	_, offset := time.Now().In(t.location).Zone()
	offsetHours := offset / 3600

	return fmt.Sprintf(
		`Manage recurring cron schedules (aka alarms, wake-ups, reminders, periodic triggers) for this session.

Actions:
- create: Create a recurring schedule. Requires "cron" — a standard 5-field cron expression (minute hour dom month dow). NO seconds field.
- list: List all active schedules for this session.
- cancel: Cancel a schedule by ID.

IMPORTANT: Write cron times in the user's local timezone (%s, UTC%+d). The system converts to UTC automatically.
Examples: "0 9 * * *" = 9:00 AM %s daily, "*/30 * * * *" = every 30 minutes.

By default a tick wakes the session with context preserved (good for stand-ups that build on prior runs).
Set "fresh": true to instead wipe the session's context on every tick and run the given "prompt" from a blank slate —
use it for independent recurring jobs so stale data from earlier runs can't leak in or bloat the context. A fresh
schedule requires a "prompt"; persist anything the next run needs via memory, since the conversation is discarded.

Use this for: recurring wake-ups, periodic monitoring, daily stand-ups, hourly checks, alarm clocks, repeated reminders, CI polling.
For one-time delays (wait 5 minutes, sleep until 9am), use the "sleep" tool instead.`,
		tzName,
		offsetHours,
		tzName,
	)
}

func (t *scheduleTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"action": {
				"type": "string",
				"enum": ["create", "list", "cancel"],
				"description": "Action to perform: create a new schedule, list existing schedules, or cancel one."
			},
			"cron": {
				"type": "string",
				"description": "For create: standard 5-field cron (minute hour dom month dow). NO seconds field. Write times in user's local timezone — the system handles UTC conversion."
			},
			"id": {
				"type": "integer",
				"description": "For cancel: the schedule ID to remove."
			},
			"prompt": {
				"type": "string",
				"description": "For create with fresh=true: the task to run on each tick. Required when fresh is set."
			},
			"fresh": {
				"type": "boolean",
				"description": "For create: when true, wipe the session's context on each tick and run \"prompt\" from a blank slate. Default false (context preserved)."
			}
		},
		"required": ["action"]
	}`)
}

func (t *scheduleTool) Execute(ctx context.Context, params json.RawMessage) (*tool.Result, error) {
	var p scheduleParams
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	if t.svc == nil {
		return nil, errors.New("schedule is not available in this context (no daemon)")
	}

	switch p.Action {
	case "create":
		return t.create(ctx, p.Cron, p.Prompt, p.Fresh)
	case "list":
		return t.list(ctx)
	case "cancel":
		return t.cancel(ctx, p.ID)
	default:
		return nil, fmt.Errorf("unknown action %q: must be create, list, or cancel", p.Action)
	}
}

func (t *scheduleTool) create(ctx context.Context, cronExpr, prompt string, fresh bool) (*tool.Result, error) {
	if cronExpr == "" {
		return nil, errors.New("cron expression is required for create action")
	}

	if fresh && prompt == "" {
		return nil, errors.New("a fresh schedule requires a prompt (the context is wiped on each tick)")
	}

	// Validate the raw expression
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	if _, err := parser.Parse(cronExpr); err != nil {
		return nil, fmt.Errorf("invalid cron expression %q: %w", cronExpr, err)
	}

	// Prepend CRON_TZ so the scheduler fires in user's timezone.
	// Store the full expression with TZ prefix — scheduler uses it as-is.
	storedExpr := fmt.Sprintf("CRON_TZ=%s %s", t.location.String(), cronExpr)

	created, err := t.svc.AddRecurring(ctx, t.sessionID, storedExpr, prompt, fresh)
	if err != nil {
		return nil, fmt.Errorf("create schedule: %w", err)
	}

	// Show next fire time in user's local timezone
	nextLocal := created.NextFire.In(t.location).Format("2006-01-02 15:04 MST")

	var sb strings.Builder
	fmt.Fprintf(&sb, "Schedule created: id=%d cron=%q next_fire=%s (%s)\n",
		created.ID, cronExpr, nextLocal, t.location.String())

	sb.WriteString("\nActive schedules:\n")

	if list, err := t.formatList(ctx); err == nil {
		sb.WriteString(list)
	}

	return &tool.Result{Output: sb.String()}, nil
}

func (t *scheduleTool) list(ctx context.Context) (*tool.Result, error) {
	list, err := t.formatList(ctx)
	if err != nil {
		return nil, err
	}

	if list == "" {
		return &tool.Result{Output: "No active schedules."}, nil
	}

	return &tool.Result{Output: list}, nil
}

func (t *scheduleTool) cancel(ctx context.Context, id int64) (*tool.Result, error) {
	if id == 0 {
		return nil, errors.New("id is required for cancel action")
	}

	if err := t.svc.RemoveSchedule(ctx, t.sessionID, id); err != nil {
		return nil, fmt.Errorf("cancel schedule: %w", err)
	}

	return &tool.Result{Output: fmt.Sprintf("Schedule %d cancelled.", id)}, nil
}

func (t *scheduleTool) formatList(ctx context.Context) (string, error) {
	schedules, err := t.svc.ListSchedules(ctx, t.sessionID)
	if err != nil {
		return "", fmt.Errorf("list schedules: %w", err)
	}

	if len(schedules) == 0 {
		return "", nil
	}

	var sb strings.Builder
	for _, s := range schedules {
		fmt.Fprintf(&sb, "- id=%d", s.ID())

		if s.CronExpr() != "" {
			fmt.Fprintf(&sb, " cron=%q", s.CronExpr())
		}

		if s.OneShotAt() != nil {
			fmt.Fprintf(&sb, " fires_at=%s", s.OneShotAt().In(t.location).Format("2006-01-02 15:04 MST"))
		}

		if s.LastFiredAt() != nil {
			fmt.Fprintf(&sb, " last_fired=%s", s.LastFiredAt().In(t.location).Format("2006-01-02 15:04 MST"))
		}

		sb.WriteByte('\n')
	}

	return sb.String(), nil
}
