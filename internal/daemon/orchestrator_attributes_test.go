package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
