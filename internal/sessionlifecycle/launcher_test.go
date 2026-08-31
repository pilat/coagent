package sessionlifecycle

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/admission"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
)

type launcherSessions struct {
	sessionstore.OrchestrationStore
	record *sessionstore.SessionRecord
}

func (s launcherSessions) GetSession(context.Context, int64) (*sessionstore.SessionRecord, error) {
	return s.record, nil
}

type launcherLinks struct{ subagent.Store }

func (launcherLinks) GetLink(context.Context, int64) (*subagent.Link, error) {
	return nil, nil
}

func TestLauncherRegistersOnceAndAppendsRacingInput(t *testing.T) {
	t.Parallel()

	runners := NewRegistry[Runner[int]]()
	started := make(chan Runner[int], 1)
	launcher := NewLauncher(
		launcherSessions{record: &sessionstore.SessionRecord{ID: 1}},
		launcherLinks{}, admission.New(), runners,
		func(context.Context, *sessionstore.SessionRecord, []int) (bool, error) { return false, nil },
		func(context.Context, int64, int64, string, int64) {},
		func(_ context.Context, _ int64, runner Runner[int]) { started <- runner },
	)

	require.NoError(t, launcher.Ensure(t.Context(), 1, "/work", 7, []int{1}))
	runner := <-started
	require.NoError(t, launcher.Ensure(t.Context(), 1, "/work", 7, []int{2}))
	assert.Equal(t, []int{1, 2}, runner.DrainInputs())
	assert.Equal(t, 1, runners.Len())

	runner.Cancel()
	runner.Complete()
}
