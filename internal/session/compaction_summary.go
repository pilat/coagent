package session

import (
	"context"
)

const (
	// legacyBackgroundSectionMarker keeps pre-rename checkpoints readable.
	legacyBackgroundSectionMarker = "\n\n# Active subagents\n"
)

// activeBackgroundSection preserves producer identities and waiting guidance across compaction.
func (s *Session) activeBackgroundSection(context.Context) string {
	return s.activeBackgroundSnapshot
}
