package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/session"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

var errInjectDown = errors.New("store is down")

func TestSessionInputVariantsValidateTheirOwnIdentity(t *testing.T) {
	tests := []struct {
		name  string
		input sessionInput
		valid bool
	}{
		{name: "exact result", input: pendingCallResultInput{
			Call: session.PendingToolCall{ID: "call-1", Name: tool.IDSleep}, Content: "done",
		}, valid: true},
		{name: "exact result without id", input: pendingCallResultInput{
			Call: session.PendingToolCall{Name: tool.IDSleep}, Content: "done",
		}},
		{name: "blocking child", input: blockingSubagentCompletionInput{
			ChildID: 2, CallID: "task-1", ActivationSeq: 1,
		}, valid: true},
		{name: "blocking child without call", input: blockingSubagentCompletionInput{ChildID: 2}},
		{name: "background child", input: backgroundSubagentCompletionInput{
			ChildID: 2, ActivationSeq: 1,
		}, valid: true},
		{name: "background child without id", input: backgroundSubagentCompletionInput{}},
		{name: "normal tick", input: scheduleTickInput{DeliveryID: "d1", Content: "tick"}, valid: true},
		{name: "empty tick", input: scheduleTickInput{}},
		{name: "fresh task", input: freshScheduleInput{DeliveryID: "d2", Prompt: "work"}, valid: true},
		{name: "empty fresh task", input: freshScheduleInput{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.input.validate()
			if tt.valid {
				require.NoError(t, err)
				return
			}

			require.Error(t, err)
		})
	}
}

func newInputsRunner(pid int64, inputs []queuedSessionInput) *runner {
	return &runner{
		cancel:    func() {},
		done:      make(chan struct{}),
		workDir:   "/tmp/test",
		projectID: pid,
		inputs:    inputs,
	}
}

// TestRunSessionIteration_InjectionFailureAbortsRun: a notification the store
// refused must not start a run — the agent would reason over a transcript that
// does not exist. Durable normal input remains pending for the retry.
func TestRunSessionIteration_InjectionFailureAbortsRun(t *testing.T) {
	ctx := context.Background()
	mgr, factory, store := newTestManager(t)
	pid := testProject(t, store, "/tmp/test")

	rec, err := mgr.sessionStore.CreateSession(ctx, pid, "fake-model", "", nil)
	require.NoError(t, err)

	sess := &mockSession{injectErr: errInjectDown}
	factory.nextSess = sess

	_, err = mgr.inboxStore.EnqueueInput(ctx, rec.ID, sessionstore.InputSourceUser, "user work")
	require.NoError(t, err)

	rs := newInputsRunner(pid, []queuedSessionInput{asyncSessionInput{
		value: freshScheduleInput{DeliveryID: "d1", Prompt: "tick"},
	}})

	var notes []sessionevent.Notification

	announced := true
	cont, hadInput := mgr.runSessionIteration(
		ctx,
		rec.ID,
		rs,
		func(n sessionevent.Notification) { notes = append(notes, n) },
		&announced,
	)

	assert.False(t, cont, "the loop must not continue")
	assert.False(t, hadInput)

	sess.mu.Lock()
	ran, closed := sess.ran, sess.closeCalled
	sess.mu.Unlock()

	assert.False(t, ran, "the session never entered its loop")
	assert.True(t, closed, "the created session is released")

	require.Len(t, notes, 2)
	assert.Equal(t, sessionevent.NotifyMessage, notes[0].Type)
	assert.Contains(t, notes[0].Message, errInjectDown.Error())
	assert.Equal(t, sessionevent.NotifyStateChanged, notes[1].Type)
	assert.Equal(t, controllerapi.StateIdle, notes[1].Status)

	input, err := mgr.inboxStore.PeekPending(ctx, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, "user work", input.RawContent)
}

// Cross-producer arrival order is not causal order. Even when a cron tick is
// queued first, the exact result that closes an older tool call must enter the
// transcript before the standalone event.
func TestPrepareSessionInputs_ExactResultsPrecedeStandaloneEvents(t *testing.T) {
	ctx := context.Background()
	mgr, _, store := newTestManager(t)
	pid := testProject(t, store, "/tmp/test")
	rec, err := mgr.sessionStore.CreateSession(ctx, pid, "fake-model", "", nil)
	require.NoError(t, err)

	sess := &mockSession{pendingCalls: []session.PendingToolCall{{ID: "sleep-1", Name: tool.IDSleep}}}
	rs := newInputsRunner(pid, []queuedSessionInput{
		asyncSessionInput{value: scheduleTickInput{DeliveryID: "d1", Content: "cron due"}},
		asyncSessionInput{value: pendingCallResultInput{
			Call:    session.PendingToolCall{ID: "sleep-1", Name: tool.IDSleep},
			Content: "timer fired",
		}},
	})

	inputs, err := mgr.prepareSessionInputs(ctx, rec.ID, rs, sess)
	require.NoError(t, err)
	assert.Len(t, inputs, 2)
	assert.Equal(t, []string{"resolve:sleep-1", "event:" + tool.IDSchedule}, sess.inputEvents)
}

func TestPrepareSessionInputs_SchedulerGetsDeferredAckBehindBlockingCall(t *testing.T) {
	ctx := context.Background()
	mgr, _, store := newTestManager(t)
	pid := testProject(t, store, "/tmp/test")
	rec, err := mgr.sessionStore.CreateSession(ctx, pid, "fake-model", "", nil)
	require.NoError(t, err)

	sess := &mockSession{pendingCalls: []session.PendingToolCall{{ID: "task-1", Name: tool.IDTask}}}
	delivery := newAwaitedSessionInput(scheduleTickInput{DeliveryID: "d1", Content: "cron due"})
	rs := newInputsRunner(pid, []queuedSessionInput{delivery})

	inputs, err := mgr.prepareSessionInputs(ctx, rec.ID, rs, sess)
	require.NoError(t, err)
	assert.Empty(t, inputs)
	assert.Empty(t, sess.inputEvents, "the tick cannot jump ahead of the blocking task result")
	outcome := <-delivery.done
	require.False(t, outcome.Applied)
	require.ErrorIs(t, outcome.Err, errSessionInputDeferred)
}
