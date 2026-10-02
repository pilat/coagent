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
func (s *svc) activeBackgroundSection(context.Context) string {
	return buildActiveBackgroundSection(s.activeProcesses, s.activeSubagents)
}
