package localapi

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"connectrpc.com/connect/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
	"github.com/pilat/coagent/internal/controllerapi"
)

func TestSessionService_ListsAndReadsOwnedRoots(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	id := inProcessRoot(t, f, "alpha")
	killed := inProcessRoot(t, f, "alpha")
	_ = inProcessRoot(t, f, "beta")
	require.NoError(t, f.store.UpdateSessionModel(t.Context(), id, "live-model", "high"))
	require.NoError(t, f.store.UpdateSessionModel(t.Context(), killed, "old-model", "low"))
	require.NoError(t, f.store.WithTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`INSERT INTO schedules(session_id,cron_expr,input_message) VALUES (?, '0 9 * * *', '')`, id)
		return err
	}))
	_, err := f.store.MarkSessionKilledWithOutput(t.Context(), killed, 0)
	require.NoError(t, err)
	require.NoError(t, f.store.SetSessionTitle(t.Context(), "alpha", id, "live title"))
	client := coagentv1connect.NewSessionServiceClient(inProcessClient(t, f, "alpha"))
	response, err := client.ListSessions(t.Context(), &coagentv1.ListSessionsRequest{})
	require.NoError(t, err)
	direct, err := f.factory.ForManager("alpha").(controllerapi.ChatFocusController).ChatSessions(t.Context(), "")
	require.NoError(t, err)
	require.Len(t, response.GetSessions(), 1)
	require.Len(t, direct, 1)
	assert.Equal(t, direct[0].ID, response.GetSessions()[0].GetId())
	assert.Equal(t, direct[0].Name, response.GetSessions()[0].GetName())
	assert.Equal(t, direct[0].Scheduled, response.GetSessions()[0].GetScheduled())
	assert.True(t, response.GetSessions()[0].GetScheduled())
	details, err := client.ListSessionDetails(t.Context(), &coagentv1.ListSessionDetailsRequest{})
	require.NoError(t, err)
	owned, err := f.factory.ForManager("alpha").ListSessions(t.Context())
	require.NoError(t, err)
	require.Len(t, details.GetSessions(), 2)
	require.Len(t, owned, 2)
	byID := make(map[int64]*coagentv1.SessionDetails)
	for _, session := range details.GetSessions() {
		byID[session.GetId()] = session
	}
	for _, session := range owned {
		got := byID[session.ID]
		require.NotNil(t, got)
		assert.Equal(t, session.WorkDir, got.GetWorkDir())
		assert.Equal(t, session.Model, got.GetModel())
		assert.Equal(t, session.ReasoningLevel, got.GetEffort())
		assert.Equal(t, session.Status, got.GetStatus())
		assert.Equal(t, session.KilledAt != nil, got.GetKilled())
		assert.Equal(t, "alpha", got.GetOwnerId())
	}
	state, err := client.GetSessionKilled(t.Context(), &coagentv1.GetSessionKilledRequest{SessionId: id})
	require.NoError(t, err)
	assert.False(t, state.GetKilled())
	_, err = f.store.MarkSessionKilledWithOutput(t.Context(), id, 0)
	require.NoError(t, err)
	state, err = client.GetSessionKilled(t.Context(), &coagentv1.GetSessionKilledRequest{SessionId: id})
	require.NoError(t, err)
	assert.True(t, state.GetKilled())
}

func TestSessionService_OrchestratorReadAndWrite(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	id := inProcessRoot(t, f, "alpha")
	client := coagentv1connect.NewSessionServiceClient(inProcessClient(t, f, "alpha"))
	mode, err := client.GetOrchestratorMode(t.Context(), &coagentv1.GetOrchestratorModeRequest{SessionId: id})
	require.NoError(t, err)
	assert.False(t, mode.GetOn())
	for _, changed := range []bool{true, false} {
		response, err := client.SetOrchestratorMode(
			t.Context(),
			&coagentv1.SetOrchestratorModeRequest{SessionId: id, On: true},
		)
		require.NoError(t, err)
		assert.Equal(t, changed, response.GetChanged())
	}
	mode, err = client.GetOrchestratorMode(t.Context(), &coagentv1.GetOrchestratorModeRequest{SessionId: id})
	require.NoError(t, err)
	assert.True(t, mode.GetOn())
	_, err = f.store.MarkSessionKilledWithOutput(t.Context(), id, 0)
	require.NoError(t, err)
	_, err = client.GetOrchestratorMode(t.Context(), &coagentv1.GetOrchestratorModeRequest{SessionId: id})
	require.Error(t, err)
	refused, ok := errors.AsType[*connect.Error](err)
	require.True(t, ok)
	assert.Equal(t, fmt.Sprintf("#%d is not a live session", id), refused.Message())
}
