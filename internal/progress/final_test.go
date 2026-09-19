package progress

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRenderFinalFromFacts_EmptyFactsReturnTextUnchanged(t *testing.T) {
	t.Parallel()

	rendered := RenderFinalFromFacts("plain answer", FinalFacts{})
	assert.Equal(t, "plain answer", rendered)
}

// A background-yield final keeps the "nothing for you to do" signal; a
// confirmed terminal stop renders badge-less — badge-less IS the your-turn
// signal.
func TestRenderFinalFromFacts_BackgroundYieldBadge(t *testing.T) {
	t.Parallel()

	yield := RenderFinalFromFacts("yielded answer", FinalFacts{
		Model:             "m",
		Iteration:         3,
		IsBackgroundYield: true,
	})
	assert.True(t, strings.HasPrefix(yield, "🟣 Background"), "yield final should open with the badge, got %q", yield)
	assert.Contains(t, yield, "yielded answer")

	confirmed := RenderFinalFromFacts("confirmed answer", FinalFacts{
		Model:     "m",
		Iteration: 3,
	})
	assert.NotContains(t, confirmed, "🟣")
}

func TestRenderFinalCompact_BackgroundYieldBadge(t *testing.T) {
	t.Parallel()

	withBadge := RenderFinalCompact(Snapshot{Model: "m", IsBackgroundYield: true})
	assert.True(t, strings.HasPrefix(withBadge, "🟣 Background"), "got %q", withBadge)

	badgeless := RenderFinalCompact(Snapshot{Model: "m"})
	assert.NotContains(t, badgeless, "🟣")
}
