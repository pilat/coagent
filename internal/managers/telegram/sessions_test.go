package telegram

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/api/coagent/v1"
)

func TestActiveSessionsExcludeKillFencesAndKilledRecords(t *testing.T) {
	assert.Equal(t,
		[]*coagentv1.SessionDetails{{Id: 1, Status: "completed"}},
		filterActiveSessions([]*coagentv1.SessionDetails{
			{Id: 1, Status: "completed"},
			{Id: 2, Status: "terminating"},
			{Id: 3, Status: "killed"},
			{Id: 4, Killed: true},
		}),
	)
}
