package schedule

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/pilat/coagent/internal/tool"
)

func (t *targetedScheduleTool) allTargets(ctx context.Context) ([]Target, error) {
	targets, err := t.targets(ctx)
	if err != nil {
		return nil, fmt.Errorf("list schedule targets: %w", err)
	}

	return append([]Target{{ID: t.sessionID, Name: "you"}}, targets...), nil
}

func (t *targetedScheduleTool) targetList(ctx context.Context) (string, error) {
	targets, err := t.allTargets(ctx)
	if err != nil {
		return "", err
	}
	var lines []string

	for _, target := range targets {
		entries, err := t.svc.ListSchedules(ctx, target.ID)
		if err != nil {
			return "", fmt.Errorf("list schedules: %w", err)
		}

		for _, entry := range entries {
			if entry.Sleep() {
				continue
			}

			line, err := t.targetLine(target, entry)
			if err != nil {
				return "", err
			}

			lines = append(lines, line)
		}
	}

	if len(lines) == 0 {
		return "No active schedules.", nil
	}

	return strings.Join(lines, "\n"), nil
}

func (t *targetedScheduleTool) targetLine(target Target, entry Entry) (string, error) {
	var when string

	var next time.Time
	if at := entry.OneShotAt(); at != nil {
		when, next = "once "+at.In(t.location).Format("2006-01-02 15:04 MST"), *at
	} else {
		expr, tz := SplitCronTZ(entry.CronExpr())
		when = fmt.Sprintf("cron %s (%s)", expr, tz)
		parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

		parsed, err := parser.Parse(entry.CronExpr())
		if err != nil {
			return "", fmt.Errorf("parse stored schedule %d: %w", entry.ID(), err)
		}

		next = parsed.Next(time.Now())
	}

	line := fmt.Sprintf(
		"- id=%d %s · %s · next %s",
		entry.ID(),
		target.Name,
		when,
		next.In(t.location).Format("2006-01-02 15:04 MST"),
	)
	if entry.Fresh() {
		line += " · fresh"
	}

	if entry.ReportTo() != 0 {
		line += " · report"
	}

	return line, nil
}

func (t *targetedScheduleTool) cancelTarget(ctx context.Context, id int64) (*tool.Result, error) {
	targets, err := t.allTargets(ctx)
	if err != nil {
		return nil, err
	}

	for _, target := range targets {
		entries, err := t.svc.ListSchedules(ctx, target.ID)
		if err != nil {
			return nil, fmt.Errorf("list schedules for cancellation: %w", err)
		}

		for _, entry := range entries {
			if entry.ID() != id || entry.Sleep() {
				continue
			}

			if err := t.svc.RemoveSchedule(ctx, target.ID, id); err != nil {
				return nil, fmt.Errorf("cancel schedule: %w", err)
			}

			return &tool.Result{Output: fmt.Sprintf("Schedule %d cancelled.", id)}, nil
		}
	}

	return nil, fmt.Errorf("schedule %d is not yours", id)
}
