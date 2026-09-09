package session

import (
	"context"
)

const (
	// backgroundSectionMarker opens the host-owned active-work subsection.
	backgroundSectionMarker = "\n\n# Active background work\n"
	// legacyBackgroundSectionMarker keeps pre-rename checkpoints readable.
	legacyBackgroundSectionMarker = "\n\n# Active subagents\n"
)

// activeBackgroundSection preserves producer identities and waiting guidance across compaction.
func (s *svc) activeBackgroundSection(ctx context.Context) string {
	var processes []ActiveProcessInfo
	if s.activeProcessesProvider != nil {
		processes = s.activeProcessesProvider(ctx)
	}

	var subagents []ActiveSubagentInfo
	if s.activeSubagentsProvider != nil {
		subagents = s.activeSubagentsProvider(ctx)
	}

	return buildActiveBackgroundSection(processes, subagents)
}
