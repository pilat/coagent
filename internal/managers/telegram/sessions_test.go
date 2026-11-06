package telegram

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/controllerapi"
)

func TestSessionLabelsDisambiguateDirectories(t *testing.T) {
	assert.Equal(t, map[int64]string{1: "one/repo", 2: "two/repo", 3: "solo"}, makeLabels([]controllerapi.SessionInfo{
		{ID: 1, WorkDir: "/one/repo"}, {ID: 2, WorkDir: "/two/repo"}, {ID: 3, WorkDir: "/solo"},
	}))
}

func TestActiveSessionsExcludeKillFencesAndKilledRecords(t *testing.T) {
	now := time.Now()
	assert.Equal(t,
		[]controllerapi.SessionInfo{{ID: 1, Status: "completed"}},
		filterActiveSessions([]controllerapi.SessionInfo{
			{ID: 1, Status: "completed"},
			{ID: 2, Status: "terminating"},
			{ID: 3, Status: "killed"},
			{ID: 4, KilledAt: &now},
		}),
	)
}
