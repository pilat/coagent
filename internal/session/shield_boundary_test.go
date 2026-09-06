package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestShieldCommandsRemainReservedAtLiveLoopBoundary(t *testing.T) {
	for _, command := range []string{shieldsUpCommand, shieldsDownCommand} {
		t.Run(command, func(t *testing.T) {
			llm := &compactionMockLLM{}
			svc := newCompactionTestSvc(llm)
			before := svc.ms.getMessages()
			var notes []string
			runner, boundary := compactCommandRunner(svc, command, &notes)
			boundary.input.ManagerOwned = true

			accepted, err := runner.drainBoundary(t.Context())
			require.NoError(t, err)

			assert.False(t, accepted)
			assert.NotNil(t, boundary.input, "the daemon still owns the durable command")
			assert.Equal(t, before, svc.ms.getMessages())
			assert.Zero(t, llm.callCount)
			assert.Empty(t, notes)
		})
	}
}

func TestShieldCommandTextWithoutManagerOwnershipRemainsNormalInput(t *testing.T) {
	svc := newCompactionTestSvc(&compactionMockLLM{})
	var notes []string
	runner, boundary := compactCommandRunner(svc, shieldsUpCommand, &notes)

	outcome, err := runner.handleBoundaryCommand(t.Context(), *boundary.input)
	require.NoError(t, err)
	assert.Equal(t, commandNotRecognized, outcome)
}
