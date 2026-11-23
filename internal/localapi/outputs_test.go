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

func TestOutputService_PagesMatchTheControllerAndBoundTheLimit(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	id := inProcessRoot(t, f, "alpha")
	after := f.append(t, id, "first")
	f.append(t, id, "second")
	f.append(t, id, "third")
	client := coagentv1connect.NewOutputServiceClient(inProcessClient(t, f, "alpha"))
	control := f.factory.ForManager("alpha").(controllerapi.ChatFocusController)
	response, err := client.ListOutput(
		t.Context(),
		&coagentv1.ListOutputRequest{SessionId: id, AfterId: after, Limit: 100},
	)
	require.NoError(t, err)
	rows, err := control.ChatOutputs(t.Context(), "", id, after, 100)
	require.NoError(t, err)
	assertOutputRows(t, rows, response.GetRows())
	entry, err := client.ReadEntry(t.Context(), &coagentv1.ReadEntryRequest{SessionId: id, Limit: 3})
	require.NoError(t, err)
	rows, newest, err := control.ChatEntry(t.Context(), "", id, 3)
	require.NoError(t, err)
	assert.Equal(t, newest, entry.GetNewestId())
	assertOutputRows(t, rows, entry.GetRows())
	for _, limit := range []int32{0, 101} {
		_, pageErr := client.ListOutput(t.Context(), &coagentv1.ListOutputRequest{SessionId: id, Limit: limit})
		_, entryErr := client.ReadEntry(t.Context(), &coagentv1.ReadEntryRequest{SessionId: id, Limit: limit})
		for _, err := range []error{pageErr, entryErr} {
			require.Error(t, err)
			refused, ok := errors.AsType[*connect.Error](err)
			require.True(t, ok)
			assert.Equal(t, connect.CodeInvalidArgument, refused.Code())
			assert.Equal(t, "limit must be 1-100", refused.Message())
		}
	}
}

func assertOutputRows(t *testing.T, expected []controllerapi.ChatOutput, actual []*coagentv1.OutputRow) {
	t.Helper()
	require.Len(t, actual, len(expected))
	for i, row := range expected {
		assert.Equal(t, row.ID, actual[i].GetId())
		assert.Equal(t, row.SessionID, actual[i].GetSessionId())
		assert.Equal(t, row.Name, actual[i].GetSessionName())
		assert.Equal(t, coagentv1.OutputType_OUTPUT_TYPE_MESSAGE, actual[i].GetType())
		assert.Equal(t, row.Content, actual[i].GetContent())
	}
}
