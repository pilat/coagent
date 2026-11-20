package sessionprompt

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderBackgroundExitNudge_ListsProcessesAndBothExits(t *testing.T) {
	t.Parallel()

	long := "curl -s 'https://example.test/search?q=" + strings.Repeat("x", 120) + "'"
	nudge := RenderBackgroundExitNudge([]LiveProcess{
		{ID: "bgp_1", Command: "go test\n  ./...", Running: 14*time.Second + 400*time.Millisecond},
		{ID: "bgp_2", Command: long, Running: 5 * time.Minute},
		{ID: "bgp_3", Running: time.Second},
	})

	assert.Contains(t, nudge, "- bgp_1 (running 14s): go test ./...\n")
	assert.Contains(t, nudge, "- bgp_2 (running 5m0s): curl -s 'https://example.test/search?q=xxx")
	assert.Contains(t, nudge, "…\n")
	assert.NotContains(t, nudge, strings.Repeat("x", 120), "a long command is cut to an excerpt")
	assert.Contains(t, nudge, "- bgp_3 (running 1s)\n")
	assert.Contains(
		t,
		nudge,
		"If you are waiting for a result: reply with one word and stop. Nothing is sent to your parent yet",
	)
	assert.Contains(t, nudge, "your answer after that is the one your parent receives")
	assert.Contains(
		t,
		nudge,
		"If you no longer need a result: cancel the process with cancel_process, then give your final answer.",
	)
	assert.Contains(
		t,
		nudge,
		"That final answer is all your parent receives, and it replaces everything you said before",
	)
	assert.Contains(t, nudge, "complete result with every finding and caveat, not only what changed since")
	waiting := strings.Index(nudge, "If you are waiting for a result")
	cancel := strings.Index(nudge, "If you no longer need a result")
	final := strings.Index(nudge, "That final answer is all your parent receives")
	require.GreaterOrEqual(t, waiting, 0)
	require.GreaterOrEqual(t, cancel, 0)
	require.GreaterOrEqual(t, final, 0)
	assert.Less(t, waiting, cancel)
	assert.Less(t, cancel, final)
}
