package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/admission"
)

func TestFinishRunnerCancellationEscapesContendedTreeFence(t *testing.T) {
	ctx := context.Background()
	mgr, _, projects := newTestManager(t)
	projectID := testProject(t, projects, "/tmp/runner-fence-cancel")
	record, err := mgr.sessionStore.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)

	unlock, err := mgr.lockSessionTree(ctx, record.ID)
	require.NoError(t, err)

	runnerCtx, cancel := context.WithCancel(ctx)
	rs := newRunner(cancel, t.TempDir(), projectID, admission.Parent, 0, false, nil)
	require.True(t, mgr.admit.TryAdmit(admission.Parent, 0))
	_, registered := mgr.runners.Register(record.ID, rs)
	require.True(t, registered)

	done := make(chan struct{})
	errored := false
	go func() {
		mgr.finishRunner(runnerCtx, record.ID, rs, &errored, false, nil)
		close(done)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case <-rs.Done():
	case <-time.After(time.Second):
		unlock()
		t.Fatal("cancelled runner did not complete while the stop fence was held")
	}

	select {
	case <-done:
		unlock()
		t.Fatal("runner finalization crossed the held tree fence")
	default:
	}

	unlock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("runner finalization did not resume after the tree fence")
	}
}
