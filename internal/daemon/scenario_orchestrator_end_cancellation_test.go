package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/orchestrator"
	"github.com/pilat/coagent/internal/sessionstore"
)

type endBlockingClient struct {
	*scriptedLLM
	entered chan struct{}
	release <-chan struct{}
	once    *sync.Once
}

func TestOrchestratorKillFromInsideATurnSurvivesCallerStop(t *testing.T) {
	// A target's lifecycle command completes even when the caller's turn is stopped midway.
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var turns scriptTurns
	root, worktrees := t.TempDir(), t.TempDir()
	h := newHarness(t, harnessOptions{
		clientFor: func(*config.Config) (llm.Client, error) {
			return &endBlockingClient{
				scriptedLLM: &scriptedLLM{respond: func(system string, messages []llmwire.Message) *llmwire.Response {
					return turns.respond(system, messages)
				}},
				entered: entered, release: release, once: &once,
			}, nil
		},
		configure: func(cfg *config.Config) {
			cfg.WorkDir = filepath.Join(root, "initial")
			require.NoError(t, os.MkdirAll(cfg.WorkDir, 0o755))
			cfg.UnifiedConfig = &config.UnifiedConfig{ProjectsRoot: root + "/.", WorktreesRoot: worktrees}
		},
	})
	t.Cleanup(func() { closeOnce(release) })
	h.startInboxWake()
	caller := modeRoot(t, h)
	target := spawnEndTarget(t, h, caller, "BLOCK_TARGET")
	waitForScenarioSignal(t, entered, "target provider entered")
	turns = scriptTurns{{marker: "END_TARGET", calls: []scriptCall{{
		id: "end-target", name: orchestrator.IDKillSession, params: fmt.Sprintf(`{"session_id":%d}`, target),
	}}, answer: "target killed"}}
	h.say(caller, "END_TARGET")
	h.waitUntil("target terminating", func() bool {
		return h.session(target).Status == sessionstore.SessionStatusTerminating
	})
	stopped := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		stopped <- h.mgr.sendToSession(h.ctx, caller, "/stop")
		close(done)
	}()
	h.waitUntil("caller stopping after cancellation", func() bool {
		return h.session(caller).Status == sessionstore.SessionStatusStopping
	})
	closeOnce(release)
	require.True(t, closedInTime(done), "caller stop must complete")
	require.NoError(t, <-stopped)
	assert.NotNil(t, h.session(target).KilledAt)
	assert.Equal(t, sessionstore.SessionStatusStopped, h.session(caller).Status)
	for _, id := range []int64{caller, target} {
		assert.NotEqual(t, sessionstore.SessionStatusStopping, h.session(id).Status)
		assert.NotEqual(t, sessionstore.SessionStatusTerminating, h.session(id).Status)
		assert.False(t, h.mgr.HasActiveLoop(id))
	}
	closed := 0
	for _, row := range h.outbox(target) {
		if row.Type == "session_closed" {
			closed++
			assert.Equal(t, "Session killed. Cancelled background processes: 0", row.Content)
		}
	}
	assert.Equal(t, 1, closed)
}

func (c *endBlockingClient) Chat(
	ctx context.Context,
	system string,
	messages []llmwire.Message,
	tools []llmwire.ToolSchema,
	options ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	// Hold the runner after cancellation so the caller can be stopped before target cleanup resumes.
	if hasUserContaining(messages, "BLOCK_TARGET") {
		c.once.Do(func() { close(c.entered) })
		<-c.release
	}
	return c.scriptedLLM.Chat(ctx, system, messages, tools, options...)
}
