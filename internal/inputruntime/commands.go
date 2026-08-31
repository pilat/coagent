package inputruntime

import (
	"context"
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/schedule"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
)

func (b *boundary) HandleWithOutput(
	ctx context.Context,
	input session.PendingInput,
	reason, content string,
) error {
	if _, owned := input.Attributes[controllerapi.SessionAttributeManagerID].(string); !owned {
		return b.Handle(ctx, input, reason)
	}

	_, err := b.store.HandleInputWithOutput(ctx, input.ID, reason, sessionstore.OutputDraft{
		SessionID: b.sessionID,
		Type:      sessionstore.OutputMessagePersistent,
		Content:   content,
	})
	if err != nil {
		return fmt.Errorf("handle session input with output: %w", err)
	}

	return nil
}

func (b *boundary) HandleSchedules(ctx context.Context, input session.PendingInput) (string, error) {
	content, err := b.renderSchedules(ctx)
	if err != nil {
		return "", err
	}

	if err := b.HandleWithOutput(ctx, input, "schedules command", content); err != nil {
		return "", err
	}

	return content, nil
}

func (b *boundary) renderSchedules(ctx context.Context) (string, error) {
	if b.schedules == nil {
		return "No schedules for this session. Ask me to add one.", nil
	}

	entries, err := b.schedules.ListSchedules(ctx, b.sessionID)
	if err != nil {
		return "", fmt.Errorf("list schedules: %w", err)
	}

	if len(entries) == 0 {
		return "No schedules for this session. Ask me to add one.", nil
	}

	lines := []string{fmt.Sprintf("## Schedules (%d)", len(entries))}
	for _, entry := range entries {
		line := fmt.Sprintf("- #%d", entry.ID())
		switch {
		case entry.CronExpr() != "":
			cronExpr, timezone := schedule.SplitCronTZ(entry.CronExpr())
			line += fmt.Sprintf(" · cron `%s` (%s)", cronExpr, timezone)
		case entry.OneShotAt() != nil:
			line += " · once " + entry.OneShotAt().UTC().Format("2006-01-02 15:04 UTC")
		}

		if entry.Fresh() {
			line += " · fresh"
		}

		if prompt := strings.TrimSpace(entry.InputMessage()); prompt != "" {
			line += " · " + prompt
		}

		lines = append(lines, line)
	}

	lines = append(lines, "", "Ask me in chat to add, change, or remove a schedule.")

	return strings.Join(lines, "\n"), nil
}
