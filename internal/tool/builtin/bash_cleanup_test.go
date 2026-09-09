package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/backgroundprocess"
	"github.com/pilat/coagent/internal/tool"
)

type removeFailProcess struct {
	backgroundprocess.Service
}

func TestBashTool_RetainsForegroundOutputAfterLifecycleCancellation(t *testing.T) {
	tests := []struct {
		name   string
		cancel func(context.Context, backgroundprocess.Service, int64) error
	}{
		{name: "stop", cancel: func(ctx context.Context, service backgroundprocess.Service, owner int64) error {
			_, err := service.CancelSessions(ctx, []int64{owner}, backgroundprocess.IntentSessionStopped)

			return err
		}},
		{name: "shutdown", cancel: func(ctx context.Context, service backgroundprocess.Service, _ int64) error {
			_, err := service.CancelAll(ctx, backgroundprocess.IntentDaemonShutdown)

			return err
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tool.WithCallID(context.Background(), "call_test")
			service, sessionID := newTestProcessService(t)
			bash := newBashTool(t.TempDir(), &bashRunnerStub{}, service, "project-1", sessionID, sessionID)
			result := make(chan *tool.Result, 1)
			errs := make(chan error, 1)

			go func() {
				params, _ := json.Marshal(bashParams{
					Command: "printf partial; sleep 30", Timeout: 5000,
				})
				value, err := bash.Execute(ctx, params)
				result <- value
				errs <- err
			}()

			var process backgroundprocess.Process
			require.Eventually(t, func() bool {
				running, err := service.Store().ListRunning(context.Background())
				if err != nil || len(running) != 1 {
					return false
				}

				process = running[0]

				return true
			}, 5*time.Second, 10*time.Millisecond)

			require.NoError(t, tt.cancel(context.Background(), service, sessionID))
			select {
			case value := <-result:
				require.NoError(t, <-errs)
				require.NotNil(t, value)
				assert.Contains(t, value.Output, "Output file: "+process.OutputPath)
			case <-time.After(5 * time.Second):
				t.Fatal("foreground cancellation did not return")
			}

			_, err := os.Stat(process.OutputPath)
			require.NoError(t, err)
		})
	}
}

func (removeFailProcess) RemoveOutput(context.Context, string) error {
	return errors.New("injected remove failure")
}

func TestBashTool_ReportsRetainedCandidateWhenCleanupFails(t *testing.T) {
	ctx := tool.WithCallID(context.Background(), "call_test")
	service, sessionID := newTestProcessService(t)
	bash := newBashTool(
		t.TempDir(), &bashRunnerStub{}, removeFailProcess{Service: service}, "project-1", sessionID, sessionID,
	)

	params, _ := json.Marshal(bashParams{Command: "printf retained"})
	result, err := bash.Execute(ctx, params)
	require.NoError(t, err)
	assert.Contains(t, result.Output, "retained")
	assert.Contains(t, result.Output, "Output file retained after cleanup failure:")
}
