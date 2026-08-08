package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/coagenthome"
	"github.com/pilat/coagent/internal/configops"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

// verdictConfig is a config valid enough for a real Stage/Commit round trip.
const verdictConfig = `providers:
    work:
        driver: anthropic
        api_key: sk-ant-verdict-0000
models:
    - id: claude-sonnet-5
      provider: work
    - id: claude-opus-5
      provider: work
`

type stubVerdictSender struct {
	calls int
	err   error
	// rec is what GetSession reports; a nil rec with no lookup error is a
	// session the store no longer has.
	rec       *sessionstore.SessionRecord
	lookupErr error
}

func (s *stubVerdictSender) DeliverPendingCallResult(
	_ context.Context, _ int64, _, _, _ string,
) (bool, error) {
	s.calls++

	return s.err == nil, s.err
}

func (s *stubVerdictSender) GetSession(_ context.Context, _ int64) (*sessionstore.SessionRecord, error) {
	if s.lookupErr != nil {
		return nil, s.lookupErr
	}

	if s.rec != nil {
		return s.rec, nil
	}

	return &sessionstore.SessionRecord{ID: 7, Status: sessionstore.SessionStatusActive}, nil
}

// commitMarker performs a real apply, leaving the marker a boot would find.
func commitMarker(t *testing.T, sessionID int64) (configops.Service, string) {
	t.Helper()

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	require.NoError(t, os.WriteFile(configPath, []byte(verdictConfig), 0o600))

	ops := configops.New(configPath, filepath.Join(dir, "secrets"))

	staged, v := ops.Stage(configops.SetDefaultModel("claude-opus-5"))
	require.True(t, v.Applied, v.Reason())

	v = ops.Commit(staged, configops.Pending{
		SessionID:  sessionID,
		ToolCallID: "c1",
		ToolName:   tool.IDSetDefaultModel,
	})
	require.True(t, v.Applied, v.Reason())

	return ops, filepath.Join(dir, coagenthome.PendingApplyFileName)
}

// The marker is the only record that a session is suspended on a config call,
// so it outlives the boot that resolved it and dies only with the delivery.
func TestDeliverApplyVerdict_MarkerClearedOnlyAfterDelivery(t *testing.T) {
	tests := []struct {
		name        string
		sessionID   int64
		deliverErr  error
		wantCalls   int
		wantCleared bool
	}{
		{
			name:        "bootstrap marker has nobody to tell",
			sessionID:   0,
			wantCalls:   0,
			wantCleared: true,
		},
		{
			name:        "delivered verdict acknowledges the marker",
			sessionID:   7,
			wantCalls:   1,
			wantCleared: true,
		},
		{
			name:        "failed delivery keeps the verdict replayable",
			sessionID:   7,
			deliverErr:  errors.New("session busy"),
			wantCalls:   1,
			wantCleared: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops, markerPath := commitMarker(t, tt.sessionID)

			pending, err := ops.LoadPending()
			require.NoError(t, err)
			require.NotNil(t, pending)

			outcome, err := ops.ResolvePending(*pending, nil)
			require.NoError(t, err)
			require.FileExists(t, markerPath, "resolving must not consume the marker")

			sender := &stubVerdictSender{err: tt.deliverErr}
			deliverApplyVerdict(context.Background(), sender, ops, &outcome)

			assert.Equal(t, tt.wantCalls, sender.calls)

			if tt.wantCleared {
				assert.NoFileExists(t, markerPath)
				return
			}

			assert.FileExists(t, markerPath, "the next boot must still find it")
		})
	}
}

// A verdict nobody can ever take must not keep the marker: a later boot re-arms
// it, and the first unrelated boot failure rolls back a config live for days.
func TestDeliverApplyVerdict_UndeliverableVerdictConsumesTheMarker(t *testing.T) {
	killedAt := time.Now()

	tests := []struct {
		name string
		rec  *sessionstore.SessionRecord
		err  error
	}{
		{name: "killed", rec: &sessionstore.SessionRecord{ID: 7, KilledAt: &killedAt}},
		{name: "stopped", rec: &sessionstore.SessionRecord{ID: 7, Status: sessionstore.SessionStatusStopped}},
		{name: "stopping", rec: &sessionstore.SessionRecord{ID: 7, Status: sessionstore.SessionStatusStopping}},
		{name: "gone from the store", err: errors.New("session 7 not found")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops, markerPath := commitMarker(t, 7)

			pending, err := ops.LoadPending()
			require.NoError(t, err)

			outcome, err := ops.ResolvePending(*pending, nil)
			require.NoError(t, err)

			before, err := os.ReadFile(ops.ConfigPath())
			require.NoError(t, err)

			sender := &stubVerdictSender{
				err: errors.New("session 7 is killed"), rec: tt.rec, lookupErr: tt.err,
			}
			deliverApplyVerdict(context.Background(), sender, ops, &outcome)

			assert.Equal(t, 1, sender.calls, "delivery is attempted before it is written off")
			assert.NoFileExists(t, markerPath, "an unanswerable verdict must not arm the next boot")

			// The boot a week later: no marker, so an unrelated failure has nothing
			// to roll the config back to.
			replay, err := ops.LoadPending()
			require.NoError(t, err)
			assert.Nil(t, replay)

			after, err := os.ReadFile(ops.ConfigPath())
			require.NoError(t, err)
			assert.Equal(t, string(before), string(after))
		})
	}
}

// A rolled-back bootstrap apply has no session to receive the verdict, which is
// why the bootstrap reads the outcome off the daemon it reconnects to.
func TestDeliverApplyVerdict_RolledBackBootstrapApplyHasNobodyToTell(t *testing.T) {
	ops, markerPath := commitMarker(t, 0)

	pending, err := ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, pending)

	outcome, err := ops.ResolvePending(*pending, errors.New("model catalog: unknown model"))
	require.NoError(t, err)
	require.True(t, outcome.RolledBack)
	require.True(t, outcome.Verdict.Failed())

	sender := &stubVerdictSender{}
	deliverApplyVerdict(context.Background(), sender, ops, &outcome)

	assert.Zero(t, sender.calls, "no session is owed this verdict")
	assert.NoFileExists(t, markerPath, "a marker nobody can consume must not arm the next boot")

	body, err := os.ReadFile(ops.ConfigPath())
	require.NoError(t, err)
	assert.Equal(t, verdictConfig, string(body), "the change is gone, and nothing on this path says so")
}

// A retry after a failed delivery clears the marker, so a transient failure
// costs a boot rather than the session.
func TestDeliverApplyVerdict_RetryAfterFailureClearsTheMarker(t *testing.T) {
	ops, markerPath := commitMarker(t, 7)

	pending, err := ops.LoadPending()
	require.NoError(t, err)

	outcome, err := ops.ResolvePending(*pending, nil)
	require.NoError(t, err)

	deliverApplyVerdict(context.Background(), &stubVerdictSender{err: errors.New("boom")}, ops, &outcome)
	require.FileExists(t, markerPath)

	replay, err := ops.LoadPending()
	require.NoError(t, err)
	require.NotNil(t, replay)

	retryOutcome, err := ops.ResolvePending(*replay, nil)
	require.NoError(t, err)
	assert.True(t, retryOutcome.Verdict.Applied, "the config on disk is the new one; the retry says so too")

	sender := &stubVerdictSender{}
	deliverApplyVerdict(context.Background(), sender, ops, &retryOutcome)

	assert.Equal(t, 1, sender.calls)
	assert.NoFileExists(t, markerPath)
}
