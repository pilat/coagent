package daemon

import (
	"context"
	"database/sql"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
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
		SessionID: ownerID, RootSessionID: rootID, ToolCallID: "process-call",
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

func processEventCount(t *testing.T, h *subagentHarness, sessionID int64) int {
	t.Helper()

	return countToolResultsFor(h.parentMessages(sessionID), "process_event")
}

func TestScenario_ProcessCompletionRevivesCompletedRootOnce(t *testing.T) {
	h := newSubagentHarnessWith(t, func(_ string, messages []llmwire.Message) *llmwire.Response {
		if hasToolResultFor(messages, "process_event") {
			return &llmwire.Response{Text: "process completion observed"}
		}

		return &llmwire.Response{Text: "unexpected activation"}
	})
	service := installScenarioProcessService(t, h)
	collector := collectEvents(h.mgr.PubSub().SubscribeAll())
	defer func() {
		collector.stop()
		h.shutdown()
	}()

	root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", map[string]any{
		"manager_id": scenarioManagerID,
	})
	require.NoError(t, err)
	require.NoError(t, h.sessStore.UpdateSessionStatus(h.ctx, root.ID, sessionstore.SessionStatusCompleted))

	process := startScenarioProcess(t, service, root.ID, root.ID, "printf 'finished\\n'")
	waitForVisibleMessage(t, collector, root.ID, "process completion observed")
	record := waitScenarioProcessState(t, h, process.ID, backgroundprocess.StateCompleted)

	assert.Equal(t, "delivered", record.DeliveryState)
	assert.Equal(t, 1, processEventCount(t, h, root.ID))

	var leakedAnnouncements, leakedContent int
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND source_key = ?`, root.ID,
		"schedule:"+process.ID+":announcement").Scan(&leakedAnnouncements))
	assert.Zero(t, leakedAnnouncements, "process events are internal model input, not scheduled manager output")
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM session_outbox
		WHERE session_id = ? AND content LIKE ?`, root.ID,
		"%Background process "+process.ID+" completed:%").Scan(&leakedContent))
	assert.Zero(t, leakedContent, "bounded process details must remain model-only")

	var generation int64
	var episodeStartedAt sql.NullTime
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT model_input_generation, episode_started_at
		FROM sessions WHERE id = ?`, root.ID).Scan(&generation, &episodeStartedAt))
	assert.Zero(t, generation, "external-call completion must not advance model-input generation")
	assert.False(t, episodeStartedAt.Valid, "external-call completion must not start a scheduled episode")

	require.NoError(t, h.mgr.processCoord.RouteRestarted(h.ctx, record, root.ID))
	h.waitUntil("duplicate completion settles without a model turn", func() bool {
		return !h.mgr.HasActiveLoop(root.ID)
	})
	assert.Equal(t, 1, processEventCount(t, h, root.ID))
}

func TestScenario_ProcessCompletionRoutesBySubagentState(t *testing.T) {
	tests := []struct {
		name       string
		status     sessionstore.SessionStatus
		wantTarget string
	}{
		{name: "suspended owner", status: sessionstore.SessionStatusSuspended, wantTarget: "child"},
		{name: "completed owner", status: sessionstore.SessionStatusCompleted, wantTarget: "root"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSubagentHarnessWith(t, func(_ string, messages []llmwire.Message) *llmwire.Response {
				if hasToolResultFor(messages, "process_event") {
					return &llmwire.Response{Text: "routed process completion"}
				}
				if hasToolResultFor(messages, "subagent_event") {
					return &llmwire.Response{Text: "subagent relayed process completion"}
				}

				return &llmwire.Response{Text: "unexpected activation"}
			})
			service := installScenarioProcessService(t, h)
			collector := collectEvents(h.mgr.PubSub().SubscribeAll())
			defer func() {
				collector.stop()
				h.shutdown()
			}()

			root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
			require.NoError(t, err)
			require.NoError(t, h.sessStore.UpdateSessionStatus(
				h.ctx, root.ID, sessionstore.SessionStatusCompleted,
			))
			childID, err := h.mgr.subagents.Create(h.ctx, subagent.Create{
				ProjectID: h.projectID, ParentID: root.ID, RootID: root.ID,
				Model: "fake-model", TaskCallID: "child-process", State: subagent.StateRunning,
			})
			require.NoError(t, err)
			require.NoError(t, h.sessStore.UpdateSessionStatus(h.ctx, childID, tt.status))

			process := startScenarioProcess(t, service, childID, root.ID, "printf 'child done\\n'")
			target := root.ID
			other := childID
			if tt.wantTarget == "child" {
				target, other = childID, root.ID
			}

			visible := "routed process completion"
			if tt.wantTarget == "child" {
				visible = "subagent relayed process completion"
			}
			waitForVisibleMessage(t, collector, root.ID, visible)
			record := waitScenarioProcessState(t, h, process.ID, backgroundprocess.StateCompleted)
			assert.Equal(t, target, record.DeliveryTargetSessionID)
			assert.Equal(t, 1, processEventCount(t, h, target))
			assert.Equal(t, 0, processEventCount(t, h, other))

			if tt.wantTarget == "root" {
				messages := h.parentMessages(root.ID)
				assert.Contains(t, lastToolResultContent(messages, "process_event"), "send_to_subagent")
			}
		})
	}
}

func TestScenario_StopAndKillCancelBackgroundProcessesWithoutWake(t *testing.T) {
	tests := []struct {
		name   string
		cancel func(context.Context, *svc, int64) error
	}{
		{name: "stop", cancel: func(ctx context.Context, manager *svc, id int64) error {
			return manager.Stop(ctx, id, 0)
		}},
		{name: "kill", cancel: func(ctx context.Context, manager *svc, id int64) error {
			return manager.Kill(ctx, id)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newSubagentHarnessWith(t, func(string, []llmwire.Message) *llmwire.Response {
				return &llmwire.Response{Text: "must not wake"}
			})
			service := installScenarioProcessService(t, h)
			defer h.shutdown()

			root, err := h.sessStore.CreateSession(h.ctx, h.projectID, "fake-model", "", nil)
			require.NoError(t, err)
			process := startScenarioProcess(t, service, root.ID, root.ID, "sleep 30")

			require.NoError(t, tt.cancel(h.ctx, h.mgr, root.ID))
			record := waitScenarioProcessState(t, h, process.ID, backgroundprocess.StateCancelled)
			assert.Equal(t, "suppressed", record.DeliveryState)
			assert.Equal(t, 0, processEventCount(t, h, root.ID))
		})
	}
}
