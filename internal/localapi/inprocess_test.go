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

func TestInProcess_CallsActAsTheBoundManager(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	client := inProcessClient(t, f, "alpha")
	sessions := coagentv1connect.NewSessionServiceClient(client)
	response, err := sessions.CreateSession(t.Context(), &coagentv1.CreateSessionRequest{WorkDir: t.TempDir()})
	require.NoError(t, err)
	record, err := f.store.GetSession(t.Context(), response.GetSessionId())
	require.NoError(t, err)
	assert.Equal(t, "alpha", record.Attributes[controllerapi.SessionAttributeManagerID])
	beta, err := f.store.CreateSubagentSession(
		t.Context(),
		record.ProjectID,
		record.ID,
		record.ID,
		"general",
		"model",
		"",
	)
	require.NoError(t, err)
	_, err = sessions.GetSessionKilled(t.Context(), &coagentv1.GetSessionKilledRequest{SessionId: beta})
	require.Error(t, err)
	assert.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	_, direct := f.factory.ForManager("alpha").(controllerapi.ChatFocusController).SessionKilled(t.Context(), beta)
	require.Error(t, direct)
	refused, ok := errors.AsType[*connect.Error](err)
	require.True(t, ok)
	assert.Equal(t, direct.Error(), refused.Message())
}

func TestInProcess_RefusalKeepsTheControllerTextByteForByte(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	id := inProcessRoot(t, f, "alpha")
	f.backend.send = func(context.Context) error { return errors.New("first line\nsecond line") }
	sessions := coagentv1connect.NewSessionServiceClient(inProcessClient(t, f, "alpha"))
	_, err := sessions.SendInput(t.Context(), &coagentv1.SendInputRequest{
		SessionId: id, Text: "hello", IdempotencyKey: "test",
	})
	require.Error(t, err)
	assert.Equal(t, connect.CodeFailedPrecondition, connect.CodeOf(err))
	refused, ok := errors.AsType[*connect.Error](err)
	require.True(t, ok)
	message := refused.Message()
	assert.Equal(t, "send resolved session message: blocked test send: first line\nsecond line", message)
	direct := f.factory.ForManager("alpha").SendSessionMessage(t.Context(), controllerapi.SessionMessageData{
		SessionID: id, Message: "hello", IdempotencyKey: "direct",
	})
	require.Error(t, direct)
	assert.Equal(t, direct.Error(), message)
}

func inProcessClient(t *testing.T, f *apiFixture, owner string) *connect.Client {
	t.Helper()
	transport, err := NewInProcess(t.Context(), f.factory).Transport(owner)
	require.NoError(t, err)
	return connect.NewClient(transport)
}

func inProcessRoot(t *testing.T, f *apiFixture, owner string) int64 {
	t.Helper()
	sessions := coagentv1connect.NewSessionServiceClient(inProcessClient(t, f, owner))
	response, err := sessions.CreateSession(
		t.Context(),
		&coagentv1.CreateSessionRequest{WorkDir: t.TempDir(), Model: "model"},
	)
	require.NoError(t, err)
	return response.GetSessionId()
}
