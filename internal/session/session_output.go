package session

import (
	"context"
	"strings"

	"github.com/pilat/coagent/internal/sessionstore"
)

func (s *svc) enqueuePersistentOutput(ctx context.Context, content string) error {
	if !s.outputEnabled {
		return nil
	}

	return s.ms.enqueueOutput(ctx, sessionstore.OutputMessagePersistent, content)
}

func (s *svc) renderSessionHelp() string {
	lines := []string{
		"## Session commands",
		"`/status` — show session status",
		"`/stop` — stop the current run",
		"`/clear` — start a fresh session",
		"`/kill` — close this session",
		"`/compact [focus]` — compact the context",
		"`/schedules` — list schedules",
		"`/budget <request>` — arm, replace, inspect, or clear a one-shot cost/wall-time checkpoint",
	}
	if s.loader == nil {
		return strings.Join(lines, "\n")
	}

	for _, skill := range s.loader.ListUserInvocableSkills() {
		line := "`/skill " + skill.Name + "`"
		if skill.Description != "" {
			line += " — " + skill.Description
		}

		lines = append(lines, line)
	}

	return strings.Join(lines, "\n")
}
