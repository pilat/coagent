package localapi

import (
	"context"
	"errors"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
	"github.com/pilat/coagent/internal/controllerapi"
)

type commandStub struct {
	controllerapi.Controller
	controllerapi.ChatFocusController
	controllerapi.LineageController
	stopped, killed, outcome []int64
	err                      error
}

func (s *commandStub) StopSession(_ context.Context, data controllerapi.SessionStopData) (int64, error) {
	s.stopped = append(s.stopped, data.SessionID)
	return data.SessionID + 1, s.err
}

func (s *commandStub) KillSession(_ context.Context, data controllerapi.SessionKillData) (int64, error) {
	s.killed = append(s.killed, data.SessionID)
	return data.SessionID + 1, s.err
}

func (s *commandStub) TurnOver(_ context.Context, id int64) (int64, bool, error) {
	s.outcome = append(s.outcome, id)
	return id + 1, true, s.err
}

func TestServer_BindsEachClientToItsCapability(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	first, second := &commandStub{}, &commandStub{}
	f.entries[0].Control = first
	f.entries[1].Control = second
	h := f.start(t)
	for i, id := range []string{"alpha", "beta"} {
		client := coagentv1connect.NewSessionServiceClient(h.client(t, "Bearer "+f.tokens[id]))
		_, err := client.StopSession(t.Context(), &coagentv1.StopSessionRequest{SessionId: int64(41 + i)})
		require.NoError(t, err)
	}
	assert.Equal(t, []int64{41}, first.stopped)
	assert.Equal(t, []int64{42}, second.stopped)
}

func TestStopKillAndTurnOutcome_PassTheSessionAndRejectMalformedRequests(t *testing.T) {
	t.Parallel()
	stub := &commandStub{}
	transport, err := NewInProcess(t.Context(), apiStubFactory{control: stub}).Transport("alpha")
	require.NoError(t, err)
	client := coagentv1connect.NewSessionServiceClient(connect.NewClient(transport))
	stop, err := client.StopSession(t.Context(), &coagentv1.StopSessionRequest{SessionId: 42})
	require.NoError(t, err)
	assert.Equal(t, int64(43), stop.GetSessionId())
	kill, err := client.KillSession(t.Context(), &coagentv1.KillSessionRequest{SessionId: 42})
	require.NoError(t, err)
	assert.Equal(t, int64(43), kill.GetSessionId())
	outcome, err := client.GetTurnOutcome(t.Context(), &coagentv1.GetTurnOutcomeRequest{SessionId: 42})
	require.NoError(t, err)
	assert.Equal(t, int64(43), outcome.GetSessionId())
	assert.True(t, outcome.GetOver())

	for _, id := range []int64{0, -1} {
		_, err := client.StopSession(t.Context(), &coagentv1.StopSessionRequest{SessionId: id})
		assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
		_, err = client.KillSession(t.Context(), &coagentv1.KillSessionRequest{SessionId: id})
		assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
		_, err = client.GetTurnOutcome(t.Context(), &coagentv1.GetTurnOutcomeRequest{SessionId: id})
		assert.Equal(t, connect.CodeInvalidArgument, connect.CodeOf(err))
	}
	assert.Equal(t, []int64{42}, stub.stopped)
	assert.Equal(t, []int64{42}, stub.killed)
	assert.Equal(t, []int64{42}, stub.outcome)

	stub.err = errors.New("the controller refused")
	_, err = client.StopSession(t.Context(), &coagentv1.StopSessionRequest{SessionId: 42})
	assert.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	require.ErrorContains(t, err, stub.err.Error())
	_, err = client.KillSession(t.Context(), &coagentv1.KillSessionRequest{SessionId: 42})
	assert.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	require.ErrorContains(t, err, stub.err.Error())
	_, err = client.GetTurnOutcome(t.Context(), &coagentv1.GetTurnOutcomeRequest{SessionId: 42})
	assert.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	assert.ErrorContains(t, err, stub.err.Error())
}
