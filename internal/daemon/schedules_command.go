package daemon

import (
	"context"
	"fmt"
	"strings"

	"github.com/pilat/coagent/internal/schedule"
)

func (s *svc) schedulesCommand(ctx context.Context, sessionID int64) (string, error) {
	if s.scheduleSvc == nil {
		return "No schedules for this session. Ask me to add one.", nil
	}

	entries, err := s.scheduleSvc.ListSchedules(ctx, sessionID)
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
			expr, zone := schedule.SplitCronTZ(entry.CronExpr())
			line += fmt.Sprintf(" · cron `%s` (%s)", expr, zone)
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
