package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/orchestrator"
)

func TestManager_SetAttributesRejectsOrchestrator(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	id := h.createRoot(managerAttrs("owner"))
	for _, value := range []any{true, false, nil} {
		err := h.mgr.SetAttributes(h.ctx, id, map[string]any{"orchestrator": value})
		require.EqualError(t, err, "orchestrator attribute is set only by /orchestrate")
		assert.NotContains(t, h.session(id).Attributes, "orchestrator")
	}
}

func TestManager_SetAttributesRejectsSpawnedBy(t *testing.T) {
	h := newHarness(t, harnessOptions{respond: trivialRespond})
	id := h.createRoot(managerAttrs("owner"))
	for _, value := range []any{int64(12), float64(12), "12", nil} {
		err := h.mgr.SetAttributes(h.ctx, id, map[string]any{"spawned_by": value})
		require.EqualError(t, err, "spawned_by attribute is set only by spawn_session")
		assert.NotContains(t, h.session(id).Attributes, "spawned_by")
	}
}

// Clearing preserves spawn authority without granting it to human-created roots.
func TestSpawnedByCopiesAcrossClear(t *testing.T) {
	h := orchestratorProjectHarness(t)
	caller := modeRoot(t, h)
	_, path := orchestratorProject(t, h, "owned")
	spawned, _, err := (orchestratorHost{s: h.mgr}).SpawnSession(h.ctx, caller, orchestrator.SpawnRequest{
		Project: path,
		Prompt:  "do work",
	})
	require.NoError(t, err)
	assert.Equal(t, caller, controllerapi.SpawnedBy(h.session(spawned).Attributes))
	replacement := clearReadTarget(t, h, spawned)
	assert.Equal(t, caller, controllerapi.SpawnedBy(h.session(replacement).Attributes))
	human := h.createRoot(managerAttrs("owner"))
	assert.Zero(t, controllerapi.SpawnedBy(h.session(human).Attributes))
	humanReplacement := clearReadTarget(t, h, human)
	assert.Zero(t, controllerapi.SpawnedBy(h.session(humanReplacement).Attributes))
}
