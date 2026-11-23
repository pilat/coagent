package localapi

import (
	"errors"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
	"github.com/pilat/coagent/internal/controllerapi"
)

func TestSessionService_TitleAndModelReachTheController(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	id := inProcessRoot(t, f, "alpha")
	client := coagentv1connect.NewSessionServiceClient(inProcessClient(t, f, "alpha"))
	_, err := client.SetSessionTitle(t.Context(), &coagentv1.SetSessionTitleRequest{SessionId: id, Title: "new title"})
	require.NoError(t, err)
	record, err := f.store.GetSession(t.Context(), id)
	require.NoError(t, err)
	assert.Equal(t, "new title", record.Attributes[controllerapi.SessionAttributeTitle])
	_, err = client.SetSessionTitle(t.Context(), &coagentv1.SetSessionTitleRequest{SessionId: id, Title: "two\nlines"})
	require.Error(t, err)
	refused, ok := errors.AsType[*connect.Error](err)
	require.True(t, ok)
	assert.Equal(t, connect.CodeFailedPrecondition, refused.Code())
	assert.Equal(t, "title must be one line of 1-80 characters", refused.Message())
	_, err = client.SetSessionModel(
		t.Context(),
		&coagentv1.SetSessionModelRequest{SessionId: id, Model: "next", Effort: "high"},
	)
	require.NoError(t, err)
	f.backend.mu.Lock()
	defer f.backend.mu.Unlock()
	assert.Equal(
		t,
		[]controllerapi.SessionSetModelData{{SessionID: id, Model: "next", ReasoningLevel: "high"}},
		f.backend.models,
	)
}
