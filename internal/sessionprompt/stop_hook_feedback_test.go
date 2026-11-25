package sessionprompt

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRenderStopHookFeedbackCarriesReason(t *testing.T) {
	assert.Equal(t,
		"A Stop hook kept you from ending your turn:\n\ncheck the result\n\n"+
			"Address it, then finish with a complete answer to the original request; your next answer replaces the previous one.",
		RenderStopHookFeedback("check the result"),
	)
}
