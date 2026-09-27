package sessionlifecycle

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

type failingMessagesSessions struct {
	sessionstore.OrchestrationStore
	err error
}

func (s failingMessagesSessions) LoadActivationOutcome(
	context.Context,
	int64,
	bool,
) (*sessionstore.ActivationOutcome, error) {
	return &sessionstore.ActivationOutcome{
		Kind:       sessionstore.ActivationFailed,
		Text:       "could not load final messages after 3 iterations",
		Diagnostic: s.err,
	}, nil
}

func TestDeriveOutcomeLoadFailureReportsError(t *testing.T) {
	t.Parallel()

	c := &completions{sessions: failingMessagesSessions{err: errors.New("db down")}}

	result, outcome, err := c.recoveredOutcome(t.Context(), 7, false)

	require.NoError(t, err)
	assert.Equal(t, "could not load final messages after 3 iterations", result.Text)
	assert.Equal(t, subagent.OutcomeError, outcome)
}
