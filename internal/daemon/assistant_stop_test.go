package daemon

import (
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
				require.Zero(t, assistantEventCount(t, h, root.ID))
				if command == stopCommand {
					assert.False(t, assistantPending(t, h, root.ID, work.ID))
				} else {
					_, err := a.Target(h.ctx, work.ID)
					require.Error(t, err)
					cards, _, err := a.List(h.ctx, 0, 100, "")
					require.NoError(t, err)
					for _, card := range cards {
						assert.NotEqual(t, work.ID, card.ID)
					}
				}
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
	assert.False(t, assistantPending(t, h, root.ID, work.ID))
	assert.Zero(t, assistantEventCount(t, h, root.ID))
	stopped, err := h.store.GetSession(h.ctx, work.ID)
	require.NoError(t, err)
	assert.Equal(t, sessionstore.SessionStatusStopped, stopped.Status)
}
