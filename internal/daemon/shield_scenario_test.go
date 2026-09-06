package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestHarnessScenario_IdleShieldRaiseIsDurableAndVisibleOnEveryCard(t *testing.T) {
	h := newSubagentHarnessWith(t, func(_ string, _ []llmwire.Message) *llmwire.Response {
		t.Fatal("shield command reached the model")
		return nil
	})
	defer h.shutdown()
	h.mgr.sandboxEnabled = true

	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	controller := newChainController(t, h)
	require.NoError(t, h.mgr.RefreshProgress(h.ctx, root.ID))
	downCard, err := controller.ClaimOutput(h.ctx)
	require.NoError(t, err)
	assert.NotContains(t, downCard.Content, "Shields")
	require.NoError(t, controller.AckOutput(h.ctx, controllerapi.OutputAckData{
		ID: downCard.ID, AttemptID: downCard.AttemptID, MessageIDs: []string{"down-card"},
	}))

	require.NoError(t, h.mgr.SendToSession(h.ctx, root.ID, "/shieldsup"))
	started, err := controller.ClaimOutput(h.ctx)
	require.NoError(t, err)
	assert.Equal(t, controllerapi.OutputMessageReplaceable, started.Type)
	assert.Equal(t, sessionstore.ShieldRaiseProgressContent, started.Content)
	assert.False(t, started.ReleasesInput)
	require.NoError(t, controller.AckOutput(h.ctx, controllerapi.OutputAckData{
		ID: started.ID, AttemptID: started.AttemptID, MessageIDs: []string{"shield-progress"},
	}))

	completed, err := controller.ClaimOutput(h.ctx)
	require.NoError(t, err)
	assert.Equal(t, controllerapi.OutputMessagePersistent, completed.Type)
	assert.Equal(t, sessionstore.ShieldRaisedContent, completed.Content)
	assert.True(t, completed.ReleasesInput)
	require.NoError(t, controller.AckOutput(h.ctx, controllerapi.OutputAckData{
		ID: completed.ID, AttemptID: completed.AttemptID, MessageIDs: []string{"shield-complete"},
	}))

	current, err := h.mgr.CurrentProgress(h.ctx, root.ID)
	require.NoError(t, err)
	assert.Contains(t, current.Rendered, "- Shields: raised")
	require.NoError(t, h.mgr.RefreshProgress(h.ctx, root.ID))
	card, err := controller.ClaimOutput(h.ctx)
	require.NoError(t, err)
	assert.Equal(t, controllerapi.OutputMessageReplaceable, card.Type)
	assert.Contains(t, card.Content, "🛡️ Shields raised")
	require.NoError(t, controller.AckOutput(h.ctx, controllerapi.OutputAckData{
		ID: card.ID, AttemptID: card.AttemptID, MessageIDs: []string{"shield-card"},
	}))

	_, err = controller.ClaimOutput(h.ctx)
	require.ErrorIs(t, err, controllerapi.ErrNoOutput)

	require.NoError(t, h.mgr.SendToSession(h.ctx, root.ID, "/shieldsdown"))
	lowered, err := controller.ClaimOutput(h.ctx)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.ShieldLoweredContent, lowered.Content)
	require.NoError(t, controller.AckOutput(h.ctx, controllerapi.OutputAckData{
		ID: lowered.ID, AttemptID: lowered.AttemptID, MessageIDs: []string{"shield-lowered"},
	}))
	require.NoError(t, h.mgr.RefreshProgress(h.ctx, root.ID))
	downAgain, err := controller.ClaimOutput(h.ctx)
	require.NoError(t, err)
	assert.NotContains(t, downAgain.Content, "Shields")
	require.NoError(t, controller.AckOutput(h.ctx, controllerapi.OutputAckData{
		ID: downAgain.ID, AttemptID: downAgain.AttemptID, MessageIDs: []string{"down-again-card"},
	}))

	messages, err := h.sessStore.LoadActiveMessages(h.ctx, root.ID)
	require.NoError(t, err)
	assert.Empty(t, messages)
}
