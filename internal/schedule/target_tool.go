package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/pilat/coagent/internal/tool"
)

type Target struct {
	ID   int64
	Name string
}

type targetedScheduleTool struct {
	*scheduleTool
	resolve func(context.Context, int64) (string, error)
	targets func(context.Context) ([]Target, error)
}

type targetedParams struct {
	scheduleParams
	Session json.RawMessage `json:"session"`
	At      string          `json:"at"`
	Report  bool            `json:"report"`
}

// NewTargetedScheduleTool limits work scheduling to roots authorized by the assistant's host.
func NewTargetedScheduleTool(sessionID int64, svc Service, loc *time.Location,
	resolve func(context.Context, int64) (string, error), targets func(context.Context) ([]Target, error),
) tool.Tool {
	if loc == nil {
		loc = time.UTC
	}

	return &targetedScheduleTool{
		scheduleTool: &scheduleTool{sessionID: sessionID, svc: svc, location: loc},
		resolve:      resolve,
		targets:      targets,
	}
}

func (t *targetedScheduleTool) Description() string {
	return "Manage your own and addressable work sessions' schedules. create takes optional session (omitted or 0 means you), " +
		"exactly one of cron (5 fields) or at (YYYY-MM-DD HH:MM), prompt, fresh and report. Times are local to " + t.location.String() + ". " +
		"Work schedules require prompt; your own require it only with fresh. report applies only to work and sends its answer back to you. " +
		"list shows your and work schedules, excluding sleeps; cancel removes an authorized non-sleep schedule by id. " +
		"Never use a schedule to poll the status or completion of already-started work."
}

func (*targetedScheduleTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{
		"action":{"type":"string","enum":["create","list","cancel"]},
		"session":{"type":"string","description":"Work session id, optionally prefixed with #; omitted or 0 means you."},
		"cron":{"type":"string","description":"Standard 5-field cron in local time."},
		"at":{"type":"string","description":"One-time local YYYY-MM-DD HH:MM, in the future."},
		"prompt":{"type":"string"},"fresh":{"type":"boolean"},"report":{"type":"boolean"},
		"id":{"type":"integer"}},"required":["action"]}`)
}

func (t *targetedScheduleTool) Execute(ctx context.Context, raw json.RawMessage) (*tool.Result, error) {
	var p targetedParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("invalid parameters: %w", err)
	}

	if t.svc == nil {
		return nil, errors.New("schedule is not available in this context (no daemon)")
	}

	switch p.Action {
	case "create":
		return t.createTarget(ctx, p)
	case "list":
		list, err := t.targetList(ctx)
		return &tool.Result{Output: list}, err
	case "cancel":
		return t.cancelTarget(ctx, p.ID)
	default:
		return nil, fmt.Errorf("unknown action %q: must be create, list, or cancel", p.Action)
	}
}

func (t *targetedScheduleTool) target(ctx context.Context, raw json.RawMessage) (Target, error) {
	id := int64(0)

	if len(raw) > 0 {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			text = string(raw)
		}

		parsed, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(text), "#"), 10, 64)
		if err != nil || parsed < 0 {
			return Target{}, errors.New("session must be a session id")
		}

		id = parsed
	}

	if id == 0 {
		return Target{ID: t.sessionID, Name: "you"}, nil
	}

	name, err := t.resolve(ctx, id)
	if err != nil {
		return Target{}, fmt.Errorf("schedule target: %w", err)
	}

	return Target{ID: id, Name: name}, nil
}

func (t *targetedScheduleTool) createTarget(ctx context.Context, p targetedParams) (*tool.Result, error) {
	target, err := t.target(ctx, p.Session)
	if err != nil {
		return nil, err
	}

	if (p.Cron == "") == (p.At == "") {
		return nil, errors.New("create requires exactly one of cron and at")
	}

	if (target.ID != t.sessionID || p.Fresh) && strings.TrimSpace(p.Prompt) == "" {
		return nil, errors.New("work-session or fresh schedule requires a prompt")
	}

	reportTo := int64(0)

	if p.Report {
		if target.ID == t.sessionID {
			return nil, errors.New("report applies to work-session schedules")
		}

		reportTo = t.sessionID
	}

	if p.At != "" && strings.TrimSpace(p.Prompt) == "" {
		p.Prompt = "Scheduled wake-up."
	}

	created, err := t.addTarget(ctx, target.ID, p, reportTo)
	if err != nil {
		return nil, err
	}

	return &tool.Result{Output: fmt.Sprintf("Schedule created: id=%d target=%s next_fire=%s", created.ID, target.Name,
		created.NextFire.In(t.location).Format("2006-01-02 15:04 MST"))}, nil
}

func (t *targetedScheduleTool) addTarget(
	ctx context.Context,
	id int64,
	p targetedParams,
	reportTo int64,
) (Created, error) {
	if p.At != "" {
		at, err := time.ParseInLocation("2006-01-02 15:04", p.At, t.location)
		if err != nil {
			return Created{}, fmt.Errorf("invalid at time: %w", err)
		}

		created, err := t.svc.AddOnce(ctx, id, at, p.Prompt, p.Fresh, reportTo)
		if err != nil {
			return Created{}, fmt.Errorf("create schedule: %w", err)
		}

		return created, nil
	}

	stored := "CRON_TZ=" + t.location.String() + " " + p.Cron

	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	if _, err := parser.Parse(stored); err != nil {
		return Created{}, fmt.Errorf("invalid cron expression %q: %w", p.Cron, err)
	}

	created, err := t.svc.AddRecurringReport(
		ctx,
		id,
		stored,
		p.Prompt,
		p.Fresh,
		reportTo,
	)
	if err != nil {
		return Created{}, fmt.Errorf("create schedule: %w", err)
	}

	return created, nil
}
