package session

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
)

// pinFixture builds a transcript whose tail ends in the pending candidate and
// its host nudge: a header, a summarizable head, then two small rows. The head
// fits the request bound while the nudge alone clears the tail floor, so the
// unpinned split lands past the candidate and the pin is load-bearing.
func pinFixture(t *testing.T) ([]llmwire.Message, int) {
	t.Helper()

	messages := []llmwire.Message{
		{Role: llmwire.RoleSystem, Content: "sys"},
		{Role: llmwire.RoleUser, Content: "task"},
		{Role: llmwire.RoleAssistant, Content: strings.Repeat("h", 8_000)},
		{Role: llmwire.RoleAssistant, Content: "first answer"},
		{Role: llmwire.RoleUser, Content: "second look " + strings.Repeat("n", 4_000)},
	}

	pin := len(messages) - 2

	return messages, pin
}

// The compaction pin keeps the pending candidate and its nudge verbatim: the
// pinned split stays at or before the candidate, while the unpinned split
// would have summarized it away.
func TestCompactionPinRetainsCandidateAndNudge(t *testing.T) {
	messages, pin := pinFixture(t)
	const window = 80_000

	cp := parseCheckpointPrefix(messages, compactionHeaderSize(messages))

	unpinned, ok := selectCheckpointSplit(messages, cp, 0, window, 0)
	require.True(t, ok, "the fixture must compact without a pin")
	assert.Greater(t, unpinned, pin, "the unpinned split must cross the candidate")

	pinned, ok := selectCheckpointSplit(messages, cp, 0, window, pin)
	require.True(t, ok, "the pinned transcript must still compact")
	assert.LessOrEqual(t, pinned, pin, "the pinned split must not cross the candidate")

	tail := messages[pinned:]
	require.GreaterOrEqual(t, len(tail), 2, "candidate and nudge stay verbatim")
	assert.Contains(t, tail[len(tail)-2].Content, "first answer", "candidate stays verbatim")
	assert.Contains(t, tail[len(tail)-1].Content, "second look", "nudge stays verbatim")

	zero, ok := selectCheckpointSplit(messages, cp, 0, window, 0)
	require.True(t, ok)
	assert.Equal(t, unpinned, zero, "pin=0 must equal unpinned behavior")
}
