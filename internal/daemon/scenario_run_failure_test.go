package daemon

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
)

type runFailureLLM struct {
	scriptedLLM
	calls atomic.Int64
}

func TestHarnessScenario_NewInputMakesRepeatedRunFailureVisible(t *testing.T) {
	client := &runFailureLLM{}
	h := newHarness(t, harnessOptions{clientFor: func(*config.Config) (llm.Client, error) {
		return client, nil
	}})
	collector := collectEvents(t, h.mgr.bus.SubscribeAll())
	defer collector.stop()
	h.startInboxWake()
	root, err := h.mgr.Send(h.ctx, h.projectID, "try the task", "fake-model", managerAttrs(scenarioManagerID))
	require.NoError(t, err)
	var generation int64
	for attempt := 1; attempt <= 2; attempt++ {
		if attempt == 2 {
			require.NoError(t, h.mgr.sendToSession(h.ctx, root, "continue"))
		}
		h.waitUntil("failed model call settled", func() bool {
			return client.calls.Load() == int64(attempt) && !h.mgr.HasActiveLoop(root)
		})
		record := h.session(root)
		assert.Equal(t, sessionstore.SessionStatusError, record.Status, "attempt %d", attempt)
		assert.Zero(t, record.Iteration, "a failed call must not advance the iteration")
		assert.Greater(t, record.ModelInputGeneration, generation)
		generation = record.ModelInputGeneration
		outputs := h.outbox(root)
		require.Len(t, outputs, attempt+1, "every new input must receive its own error notice after session_opened")
		assert.Contains(t, outputs[attempt].Content, fmt.Sprintf("scripted provider failure %d", attempt))
		collector.waitIdleAfter(root, outputs[attempt].Content)
	}
	outputs := h.outbox(root)
	assert.Equal(t, []string{outputs[1].Content, outputs[2].Content}, visibleEventMessages(collector.snapshot(), root))
	assertHarnessTrace(t, h, "repeated_run_failure.json", collector.snapshot(), root)
}

func TestHarnessModel_RunFailureNoticePerInputAcrossRebuilds(t *testing.T) {
	client := &runFailureLLM{}
	h := newHarness(t, harnessOptions{clientFor: func(*config.Config) (llm.Client, error) {
		return client, nil
	}})
	h.startInboxWake()
	var root int64
	var noticeAttempts []int
	for step, command := range []string{"input", "rebuild", "rebuild", "input", "rebuild", "input"} {
		before := h.outbox(root)
		if command == "input" {
			noticeAttempts = append(noticeAttempts, step+1)
			if root == 0 {
				var err error
				root, err = h.mgr.Send(
					h.ctx,
					h.projectID,
					"try the task",
					"fake-model",
					managerAttrs(scenarioManagerID),
				)
				require.NoError(t, err)
			} else {
				require.NoError(t, h.mgr.sendToSession(h.ctx, root, "continue"))
			}
		} else {
			replayFailedRun(t, h, root)
		}
		h.waitUntil("failed run settled", func() bool {
			return client.calls.Load() == int64(step+1) && !h.mgr.HasActiveLoop(root)
		})
		record := h.session(root)
		assert.Equal(t, sessionstore.SessionStatusError, record.Status)
		assert.Zero(t, record.Iteration)
		assert.Equal(t, int64(len(noticeAttempts)), record.ModelInputGeneration)
		outputs := h.outbox(root)
		require.Len(t, outputs, len(noticeAttempts)+1)
		for notice, attempt := range noticeAttempts {
			assert.Equal(t, fmt.Sprintf("run:%d:0:error", notice+1), outputs[notice+1].SourceKey)
			assert.Contains(t, outputs[notice+1].Content, fmt.Sprintf("scripted provider failure %d", attempt))
		}
		if command == "rebuild" {
			assert.Equal(t, before, outputs)
		}
	}
}

func (c *runFailureLLM) Chat(
	context.Context,
	string,
	[]llmwire.Message,
	[]llmwire.ToolSchema,
	...llmwire.ChatOption,
) (*llmwire.Response, error) {
	return nil, fmt.Errorf("scripted provider failure %d", c.calls.Add(1))
}

func replayFailedRun(t *testing.T, h *harness, root int64) {
	t.Helper()

	sess, cleanup, err := h.mgr.openSession(h.ctx, root, h.mgr.build.Config.WorkDir, h.session(root), false)
	require.NoError(t, err)
	defer cleanup()
	defer sess.Close()
	_, err = sess.Run(h.ctx)
	require.ErrorContains(t, err, "scripted provider failure")
}
