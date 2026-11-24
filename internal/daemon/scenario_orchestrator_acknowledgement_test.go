package daemon

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/orchestrator"
)

func TestOrchestratorAcknowledgesUnreadSignal(t *testing.T) {
	for _, id := range []string{orchestrator.IDSendToSession, orchestrator.IDSubscribe} {
		t.Run(id, func(t *testing.T) {
			h := newHarness(t, harnessOptions{respond: trivialRespond})
			h.startInboxWake()
			watcher := modeRoot(t, h)
			target := h.createRoot(nil)
			host := orchestratorHost{s: h.mgr}
			_, _, err := host.SendToSession(h.ctx, watcher, target, "first")
			require.NoError(t, err)
			assertAnswerSignal(t, h, watcher, target)
			waitSignalTurn(t, h, watcher, "done")
			for _, candidate := range orchestrator.NewTools(host, watcher) {
				if candidate.ID() != id {
					continue
				}

				result, err := candidate.Execute(h.ctx, json.RawMessage(fmt.Sprintf(
					`{"session_id":%d,"message":"follow-up before read"}`, target)))
				require.NoError(t, err)
				assert.Contains(t, result.Output, fmt.Sprintf("A signal for session #%d is still unread.", target))
				assert.Contains(t, result.Output, fmt.Sprintf(
					"Read it with read_session (session_id %d) before another signal can come.", target))
				assert.NotContains(t, result.Output, "You get one signal")
				return
			}

			t.Fatal("orchestrator tool missing")
		})
	}
}
