package daemon

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/registry"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestHarnessScenario_AssistantUserCancellationWithoutRunner(t *testing.T) {
	for _, state := range []string{"queued", "yielded"} {
		for _, command := range []string{stopCommand, killCommand} {
			t.Run(state+command, func(t *testing.T) {
				ready := make(chan struct{})
				t.Cleanup(func() { closeOnce(ready) })
				h := newHarness(t, harnessOptions{respond: func(system string, _ []llmwire.Message) *llmwire.Response {
					if strings.HasPrefix(system, registry.AssistantPrompt) {
						return textReply("observed completion")
					}

					<-ready

					return textReply("yielded answer")
				}})
				root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
				require.NoError(t, err)
				work, err := h.store.CreateSession(
					h.ctx, h.projectID, "fake-model", "", managerAttrs(scenarioManagerID),
				)
				require.NoError(t, err)

				if state == "queued" {
					for range maxTotal {
						require.True(t, h.mgr.runners.tryAdmit(false, 0))
					}

					t.Cleanup(func() {
						for range maxTotal {
							h.mgr.runners.release(false, 0)
						}
					})
				}

				a := assistantFor(h, root.ID)
				require.NoError(t, a.Send(h.ctx, work.ID, "work to cancel"))

				if state == "yielded" {
					service := installScenarioProcessService(t, h)
					startScenarioProcess(t, service, work.ID, work.ID, "sleep 60")
					close(ready)
					h.waitUntil("work yielded", func() bool { return !h.mgr.HasActiveLoop(work.ID) })
				} else {
					require.Equal(t, 1, h.queueLen())
				}

				require.False(t, h.mgr.HasActiveLoop(work.ID))
				require.Zero(t, assistantEventCount(t, h, root.ID))
				require.NoError(t, h.mgr.sendToSession(h.ctx, work.ID, command))
				require.Equal(t, 1, assistantEventCount(t, h, root.ID))

				var body string
				require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT raw_content FROM session_inbox
					WHERE session_id = ? AND source = 'agent'`, root.ID).Scan(&body))
				cause := "stopped"
				if command == killCommand {
					cause = "killed"
				}

				assert.Contains(t, body, fmt.Sprintf("(%d) failed: %s", work.ID, cause))
				if state == "yielded" {
					assert.Contains(t, body, "yielded answer", "what the session said before it was stopped is kept")
				}
				assert.Zero(t, unobservedCount(t, h, work.ID))
				require.NoError(t, h.mgr.sweepAssistantObservations(h.ctx))
				assert.Equal(t, 1, assistantEventCount(t, h, root.ID))
			})
		}
	}
}

func TestHarnessScenario_AssistantStopSuppressesFailure(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	h := newHarness(t, harnessOptions{respond: func(_ string, _ []llmwire.Message) *llmwire.Response {
		closeOnce(entered)
		<-release

		return textReply("cancelled answer")
	}})
	t.Cleanup(func() { closeOnce(release) })
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	work, err := h.store.CreateSession(h.ctx, h.projectID, "fake-model", "", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	h.startInboxWake()
	a := assistantFor(h, root.ID)
	require.NoError(t, a.Send(h.ctx, work.ID, "start work"))
	requireSignal(t, entered)
	require.NoError(t, a.Stop(h.ctx, work.ID))
	close(release)
	h.waitUntil("stopped work joined", func() bool { return !h.mgr.HasActiveLoop(work.ID) })
	assert.Zero(t, unobservedCount(t, h, work.ID))
	assert.Zero(t, assistantEventCount(t, h, root.ID))
	stopped, err := h.store.GetSession(h.ctx, work.ID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusStopped, stopped.Status)
}
