package builtin

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/tool"
)

func TestCancelProcessTool_CancelsOwnedProcessAndReleasesSlot(t *testing.T) {
	bash, service := newTestBashTool(t)
	ctx := tool.WithCallID(context.Background(), "start-cancellable")
	started, err := bash.Execute(ctx, []byte(`{"command":"sleep 30","background":true}`))
	require.NoError(t, err)
	processID, ok := started.Metadata[metaKeyProcessID].(string)
	require.True(t, ok)

	params, err := json.Marshal(cancelProcessParams{ProcessID: processID})
	require.NoError(t, err)
	result, err := newCancelProcessTool(service, 1).Execute(t.Context(), params)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "released its process slot")
	assert.Contains(t, result.Output, "No completion will arrive")

	process, err := service.Store().GetProcess(t.Context(), processID)
	require.NoError(t, err)
	assert.Equal(t, backgroundprocess.StateCancelled, process.State)
	assert.Equal(t, backgroundprocess.IntentAgentCancelled, process.HostIntent)
}

func TestCancelProcessTool_RejectsAnotherSessionProcess(t *testing.T) {
	bash, service := newTestBashTool(t)
	ctx := tool.WithCallID(context.Background(), "start-owned")
	started, err := bash.Execute(ctx, []byte(`{"command":"sleep 30","background":true}`))
	require.NoError(t, err)
	processID, ok := started.Metadata[metaKeyProcessID].(string)
	require.True(t, ok)

	params, err := json.Marshal(cancelProcessParams{ProcessID: processID})
	require.NoError(t, err)
	_, err = newCancelProcessTool(service, 2).Execute(t.Context(), params)
	require.ErrorContains(t, err, "not found for this agent")

	process, err := service.Store().GetProcess(t.Context(), processID)
	require.NoError(t, err)
	assert.Equal(t, backgroundprocess.StateRunning, process.State)
}

func TestRegisterCoreTools_ExposesCancelOnlyWithProcessLifecycle(t *testing.T) {
	service, sessionID := newTestProcessService(t)
	registry := tool.NewRegistry()
	mutator, err := newFileMutator(false, nil)
	require.NoError(t, err)

	registerCoreTools(
		registry, t.TempDir(), nil, nil, nil, nil, nil, nil, mutator, nil,
		service, "project-1", sessionID, sessionID,
	)
	assert.NotNil(t, registry.Get(tool.IDCancelProcess))
}
