package progress

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRenderFinalFromFacts_EmptyFactsReturnTextUnchanged(t *testing.T) {
	rendered := RenderFinalFromFacts("plain answer", FinalFacts{})
	assert.Equal(t, "plain answer", rendered)
}
