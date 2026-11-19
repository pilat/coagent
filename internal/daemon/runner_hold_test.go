package daemon

import (
	"context"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionprompt"
)

// runnerHold blocks one session's runner in its own model client.
// Release ends the hold only after cancellation.
type runnerHold struct {
	*scriptedLLM
	entered     chan struct{}
	cancelled   chan struct{}
	release     chan struct{}
	hadDeadline bool
}

func (c *runnerHold) Chat(
	ctx context.Context,
	_ string,
	_ []llmwire.Message,
	_ []llmwire.ToolSchema,
	_ ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	_, c.hadDeadline = ctx.Deadline()
	close(c.entered)
	<-ctx.Done()
	close(c.cancelled)
	<-c.release
	return nil, ctx.Err()
}

// The hold takes effect on the runner's next model step;
// a blocked HTTP handler cannot hold a cancelled runner.
func (h *harness) holdRunner(sessionID int64) *runnerHold {
	h.t.Helper()
	rs, ok := h.mgr.runners.load(sessionID)
	require.True(h.t, ok, "session %d has no runner", sessionID)
	live := rs.Service()
	require.NotNil(h.t, live, "session %d runner has no live session", sessionID)
	hold := &runnerHold{
		scriptedLLM: &scriptedLLM{},
		entered:     make(chan struct{}),
		cancelled:   make(chan struct{}),
		release:     make(chan struct{}),
	}
	h.t.Cleanup(func() { closeOnce(hold.release) })
	live.SwitchModel(hold, sessionprompt.ModelSection{ID: h.session(sessionID).Model})
	return hold
}
