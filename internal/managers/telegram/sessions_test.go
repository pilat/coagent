package telegram

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/controllerapi"
)

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
