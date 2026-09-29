package daemon

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessioncalls"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

func TestAmbiguousStoredCallBlocksAdmissionAndSettlement(t *testing.T) {
	store, links, rec := externalCallFixture(t, tool.IDTask)
	duplicate, err := json.Marshal([]llmwire.ToolCall{{ID: "call", Name: tool.IDTask}})
	require.NoError(t, err)
	duplicateID, err := store.InsertMessage(t.Context(), rec.ID, &transcript.Message{
		Role: llmwire.RoleAssistant, ToolCalls: duplicate,
	})
	require.NoError(t, err)

	for range 2 {
		mgr := &svc{sessionStore: store}
		_, err = mgr.ensureRunnerStartable(t.Context(), rec, nil)
		require.ErrorIs(t, err, sessioncalls.ErrAmbiguousCallID)

		owner := newExternalCalls(nil, store, store, store, nil, links, nil).(*externalCalls)
		owner.staged.stage(rec.ID, "call", tool.IDTask)
		_, err = owner.CloseOrphans(t.Context(), rec)
		require.ErrorIs(t, err, sessioncalls.ErrAmbiguousCallID)
		_, err = owner.CloseInterrupted(t.Context(), rec)
		require.ErrorIs(t, err, sessioncalls.ErrAmbiguousCallID)
		require.NoError(t, owner.SettleStopped(t.Context(), rec.ID))
		assert.Empty(t, owner.staged.forSession(rec.ID))
	}

	stored, err := store.LoadActiveMessages(t.Context(), rec.ID)
	require.NoError(t, err)
	assert.Len(t, stored, 2, "stop must leave both ambiguous calls unresolved for manual repair")
	for _, message := range stored {
		assert.Equal(t, llmwire.RoleAssistant, message.Role)
	}

	require.NoError(t, store.MarkCompacted(t.Context(), []int64{duplicateID}))
	mgr := &svc{sessionStore: store}
	_, err = mgr.ensureRunnerStartable(t.Context(), rec, nil)
	require.NoError(t, err)
}
