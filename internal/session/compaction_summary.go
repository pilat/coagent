package session

import (
	"context"
)

// backgroundSectionMarker opens the host-owned active-background subsection
// inside a marked summary. It is host vocabulary, never part of the next
// incremental model-summary anchor.
const backgroundSectionMarker = "\n\n# Active subagents\n"

// activeBackgroundSection renders the children still running, so a later
// "subagent #42 completed" lands where #42 still means something.
func (s *svc) activeBackgroundSection(ctx context.Context) string {
	if s.activeSubagentsProvider == nil {
		return ""
	}

	return buildActiveSubagentsSection(s.activeSubagentsProvider(ctx))
}
