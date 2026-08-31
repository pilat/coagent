package sessionlifecycle

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/admission"
)

func TestRunnerOwnsInputAndCompletionLifecycle(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	runner := NewRunner(cancel, "/work", 7, admission.Child, 3, true, []int{1})
	runner.AppendInput(2)
	assert.Equal(t, []int{1, 2}, runner.DrainInputs())
	assert.Empty(t, runner.DrainInputs())
	assert.False(t, runner.HasRun())
	runner.MarkRun()
	assert.True(t, runner.HasRun())

	runner.Cancel()
	require.Error(t, ctx.Err())
	runner.Complete()
	<-runner.Done()
}
