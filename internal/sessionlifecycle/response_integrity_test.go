package sessionlifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/transcript"
)

type failingMessagesSessions struct {
	sessionstore.OrchestrationStore
	err error
}

func (s failingMessagesSessions) LoadActiveMessages(context.Context, int64) ([]*transcript.Message, error) {
	return nil, s.err
}

func TestDeriveOutcomeLoadFailureReportsError(t *testing.T) {
	t.Parallel()

	c := &completions{sessions: failingMessagesSessions{err: errors.New("db down")}}

	result, outcome := c.deriveOutcome(t.Context(), 7, 3, false, false, 0)

	assert.Equal(t, "could not load final messages after 3 iterations", result)
	assert.Equal(t, subagent.OutcomeError, outcome)
}
