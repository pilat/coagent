package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/configapply"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/subagent"
	"github.com/pilat/coagent/internal/tool"
	"github.com/pilat/coagent/internal/transcript"
)

var errCallSettlement = errors.New("injected call settlement failure")

type failingCallTranscript struct {
	sessionstore.RuntimeStore
	failLoad   atomic.Bool
	failInsert atomic.Bool
}

type blockedCallTranscript struct {
	entered chan struct{}
	release chan struct{}
}

func TestExternalCallsOrphanAdoptionDoesNotSurviveFailedSettlement(t *testing.T) {
	for _, failure := range []string{"open", "resolve"} {
		t.Run(failure, func(t *testing.T) {
			t.Parallel()
			store, links, record := externalCallFixture(t, tool.IDTask)
			runtimeStore := &failingCallTranscript{RuntimeStore: store}
			runtimeStore.failLoad.Store(failure == "open")
			runtimeStore.failInsert.Store(failure == "resolve")
			owner := newExternalCalls(nil, store, store, runtimeStore, nil, links, nil)

			_, err := owner.CloseOrphans(t.Context(), record)
			require.ErrorIs(t, err, errCallSettlement)
			assert.Empty(t, pendingCallsOf(t, owner, record.ID), "failed adoption must leave no producer")

			closed, err := owner.CloseOrphans(t.Context(), record)
			require.NoError(t, err)
			assert.Equal(t, 1, closed)
			closed, err = owner.CloseOrphans(t.Context(), record)
			require.NoError(t, err)
			assert.Zero(t, closed)
			messages, err := store.LoadActiveMessages(t.Context(), record.ID)
			require.NoError(t, err)
			require.Len(t, messages, 2)
			assert.Equal(t, "call", messages[1].ToolCallID)
			assert.Equal(t, orphanedCallNotice(tool.IDTask), messages[1].Content)
		})
	}
}

func TestExternalCallsRejectedResultRetainsOwnershipUntilPersisted(t *testing.T) {
	t.Parallel()
	store, links, record := externalCallFixture(t, tool.IDConfigEdit)
	ops := newTestConfigOps(t, t.TempDir())
	applier := configapply.New(failingCommitOps{ops}, func() { t.Error("rejected apply restarted") })
	runtimeStore := &failingCallTranscript{RuntimeStore: store}
	runtimeStore.failInsert.Store(true)
	var result pendingCallResultInput
	owner := newExternalCalls(applier, store, store, runtimeStore, nil, links,
		func(_ context.Context, id int64, input pendingCallResultInput) error {
			assert.Equal(t, record.ID, id)
			result = input
			return nil
		},
	)
	staged, verdict := ops.StageDocument([]byte(configHarnessCandidate))
	require.False(t, verdict.Failed())
	require.True(t, owner.StageApply(record.ID, "call", tool.IDConfigEdit, staged))
	owner.Apply(t.Context(), record.ID, func() bool { return false })
	require.Contains(t, result.Content, "rejected")

	projection, err := session.OpenTranscript(t.Context(), runtimeStore, nil, record.ID,
		pendingCallsOf(t, owner, record.ID))
	require.NoError(t, err)
	_, err = owner.Resolve(t.Context(), record.ID, projection, result)
	require.ErrorIs(t, err, errCallSettlement)
	assert.Equal(t, tool.IDConfigEdit, pendingCallsOf(t, owner, record.ID)["call"])

	projection, err = session.OpenTranscript(t.Context(), runtimeStore, nil, record.ID,
		pendingCallsOf(t, owner, record.ID))
	require.NoError(t, err)
	applied, err := owner.Resolve(t.Context(), record.ID, projection, result)
	require.NoError(t, err)
	assert.True(t, applied)
	assert.Empty(t, pendingCallsOf(t, owner, record.ID))
	messages, err := store.LoadActiveMessages(t.Context(), record.ID)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	assert.Equal(t, result.Content, messages[1].Content)
}

func TestExternalCallsUnbackedApplyObservesShutdownAfterTranscriptRead(t *testing.T) {
	t.Parallel()
	store, links, record := externalCallFixture(t, tool.IDConfigEdit)
	applier := configapply.New(newTestConfigOps(t, t.TempDir()), func() { t.Error("unbacked apply restarted") })
	reader := &blockedCallTranscript{entered: make(chan struct{}), release: make(chan struct{})}
	var shuttingDown atomic.Bool
	var deliveries atomic.Int64
	owner := newExternalCalls(applier, store, reader, store, nil, links,
		func(context.Context, int64, pendingCallResultInput) error {
			deliveries.Add(1)
			return nil
		},
	)
	require.True(t, owner.StageApply(record.ID, "call", tool.IDConfigEdit, &configops.Staged{}))
	done := make(chan struct{})
	go func() {
		owner.Apply(t.Context(), record.ID, shuttingDown.Load)
		close(done)
	}()

	select {
	case <-reader.entered:
	case <-time.After(time.Second):
		close(reader.release)
		t.Fatal("apply did not reach transcript read")
	}
	shuttingDown.Store(true)
	close(reader.release)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("apply did not finish after transcript read")
	}

	assert.Zero(t, deliveries.Load())
	assert.Empty(t, pendingCallsOf(t, owner, record.ID))
	assert.True(t, owner.StageApply(record.ID, "later", tool.IDConfigEdit, &configops.Staged{}))
	owner.Abandon(t.Context(), record.ID)
}

func (s *failingCallTranscript) LoadActiveMessages(ctx context.Context, id int64) ([]*transcript.Message, error) {
	if s.failLoad.Swap(false) {
		return nil, errCallSettlement
	}
	return s.RuntimeStore.LoadActiveMessages(ctx, id)
}

func (s *failingCallTranscript) InsertToolResultSetOnce(
	ctx context.Context,
	id int64,
	entries []sessionstore.ToolResultEntry,
) ([]int64, [][]*sessionstore.OutputCommit, error) {
	if s.failInsert.Swap(false) {
		return nil, nil, errCallSettlement
	}
	return s.RuntimeStore.InsertToolResultSetOnce(ctx, id, entries)
}

func (s *blockedCallTranscript) LoadActiveMessages(context.Context, int64) ([]*transcript.Message, error) {
	close(s.entered)
	<-s.release
	return nil, errCallSettlement
}

func externalCallFixture(t *testing.T, name string) (sessionstore.Store, subagent.Store, *sessionstore.SessionRecord) {
	t.Helper()
	store, links, _, projectID := newTestLinkStore(t)
	record, err := store.CreateSession(t.Context(), projectID, "fake-model", "", nil)
	require.NoError(t, err)
	calls, err := json.Marshal([]llmwire.ToolCall{{ID: "call", Name: name}})
	require.NoError(t, err)
	_, err = store.InsertMessage(t.Context(), record.ID, &transcript.Message{
		Role: llmwire.RoleAssistant, ToolCalls: calls,
	})
	require.NoError(t, err)
	return store, links, record
}
