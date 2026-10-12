package sessionprompt

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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
	assert.Contains(t, nudge, "Your parent receives only your final answer, and it replaces everything you said before")
	assert.Contains(t, nudge, "complete result with every finding and caveat, not only what changed since")
	assert.Contains(t, nudge, "cancel it with cancel_process, then give that final answer")
	assert.Contains(t, nudge, "end your turn now: you are resumed when the process completes")
	assert.Contains(t, nudge, "only then does your parent receive your result")
}
