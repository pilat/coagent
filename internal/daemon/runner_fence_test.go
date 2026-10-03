package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFinishRunnerCancellationEscapesContendedTreeFence(t *testing.T) {
	ctx := context.Background()
	mgr, _, projects := newTestManager(t)
	projectID := testProject(t, projects, "/tmp/runner-fence-cancel")
	record, err := mgr.store.CreateSession(ctx, projectID, "fake-model", "", nil)
	require.NoError(t, err)

	unlock, err := mgr.lockSessionTree(ctx, record.ID)
	require.NoError(t, err)

	runnerCtx, cancel := context.WithCancel(ctx)
	rs := newRunner(cancel, t.TempDir(), record, waitingRunner{sessionID: record.ID}, false)
	require.True(t, mgr.runners.tryAdmit(false, 0))
	_, registered := mgr.runners.register(rs)
	require.True(t, registered)

	done := make(chan struct{})
	go func() {
		mgr.finishRunner(runnerCtx, rs, runOutcome{}, nil)
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
