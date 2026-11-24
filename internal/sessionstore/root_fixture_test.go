package sessionstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore/sessionstoretest"
)

func TestRootFixtureMatchesTheStoreDefaults(t *testing.T) {
	t.Parallel()
	_, db, project := newTestStore(t)
	id := sessionstoretest.BareRoot(t, db, project, "m", "", nil)
	var agentType, reasoningLevel, attributes string
	var parentID int64
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT agent_type, reasoning_level, attributes, parent_id
		FROM sessions WHERE id = ?`, id).Scan(&agentType, &reasoningLevel, &attributes, &parentID))
	assert.Equal(t, rootAgentType, agentType)
	assert.Equal(t, defaultReasoningLevel, reasoningLevel)
	assert.Equal(t, "{}", attributes)
	assert.Zero(t, parentID)
}
