package daemon

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
)

func installScenarioProcessService(t *testing.T, h *subagentHarness) backgroundprocess.Service {
	t.Helper()

	service := backgroundprocess.NewService(h.mgr.processStore, backgroundprocess.Options{
		OutputDir: t.TempDir(),
		OnCompletion: func(ctx context.Context, completion backgroundprocess.Completion) {
			h.mgr.routeProcessCompletion(ctx, completion)
		},
	})
	h.mgr.processSvc = service

	return service
}

func startScenarioProcess(
	t *testing.T,
	service backgroundprocess.Service,
	ownerID, rootID int64,
	command string,
) backgroundprocess.Process {
	t.Helper()

	record, err := service.Start(context.Background(), backgroundprocess.Spec{
		ProjectDir: "project-test", SessionID: ownerID, RootSessionID: rootID, ToolCallID: "process-call",
		Deadline: 30 * time.Second, Advertise: true,
	}, func(ctx context.Context) (*exec.Cmd, error) {
		return exec.CommandContext(ctx, "sh", "-c", command), nil
	})
	require.NoError(t, err)

	return record
}

func waitScenarioProcessState(
	t *testing.T,
	h *subagentHarness,
	processID string,
	want backgroundprocess.State,
) backgroundprocess.Process {
	t.Helper()

	var record backgroundprocess.Process
	require.Eventually(t, func() bool {
		current, err := h.mgr.processStore.GetProcess(context.Background(), processID)
		if err != nil {
			return false
		}

		record = current

		return current.State == want
	}, 5*time.Second, 10*time.Millisecond)

	return record
}
