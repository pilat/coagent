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

func TestChatService_PassesFocusAndCursorThroughTheController(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	ctx := t.Context()
	id := inProcessRoot(t, f, "alpha")
	second := inProcessRoot(t, f, "alpha")
	record, err := f.store.GetSession(ctx, id)
	require.NoError(t, err)
	foreign, err := f.store.CreateSubagentSession(ctx, record.ProjectID, id, id, "general", "model", "")
	require.NoError(t, err)
	client := inProcessClient(t, f, "alpha")
	chat := coagentv1connect.NewChatServiceClient(client)
	control := f.factory.ForManager("alpha").(controllerapi.ChatFocusController)
	_, err = chat.BindChat(ctx, &coagentv1.BindChatRequest{ChatId: "chat"})
	require.NoError(t, err)
	assertChatState(t, chat, control, 0, 0, false)
	_, err = chat.SetChatFocus(ctx, &coagentv1.SetChatFocusRequest{ChatId: "chat", SessionId: id})
	require.NoError(t, err)
	assertChatState(t, chat, control, id, 0, false)
	newest := f.append(t, id, "entry")
	entry, err := coagentv1connect.NewOutputServiceClient(client).
		ReadEntry(ctx, &coagentv1.ReadEntryRequest{SessionId: id, Limit: 3})
	require.NoError(t, err)
	assert.Equal(t, newest, entry.GetNewestId())
	for _, expected := range []bool{true, false} {
		response, err := chat.EnterChat(
			ctx,
			&coagentv1.EnterChatRequest{ChatId: "chat", SessionId: id, NewestId: newest},
		)
		require.NoError(t, err)
		assert.Equal(t, expected, response.GetEntered())
		assertChatState(t, chat, control, id, newest, true)
	}
	output := f.append(t, id, "later")
	for _, expected := range []bool{true, false} {
		response, err := chat.AdvanceChat(
			ctx,
			&coagentv1.AdvanceChatRequest{ChatId: "chat", SessionId: id, OutputId: output},
		)
		require.NoError(t, err)
		assert.Equal(t, expected, response.GetAdvanced())
		assertChatState(t, chat, control, id, output, true)
	}
	reset, err := chat.ResetChatFocus(ctx, &coagentv1.ResetChatFocusRequest{ChatId: "chat", FromSessionId: second})
	require.NoError(t, err)
	assert.False(t, reset.GetCleared())
	assertChatState(t, chat, control, id, output, true)
	reset, err = chat.ResetChatFocus(ctx, &coagentv1.ResetChatFocusRequest{ChatId: "chat", FromSessionId: id})
	require.NoError(t, err)
	assert.True(t, reset.GetCleared())
	assertChatState(t, chat, control, 0, 0, false)
	_, err = chat.SetChatFocus(ctx, &coagentv1.SetChatFocusRequest{ChatId: "chat", SessionId: foreign})
	require.Error(t, err)
	direct := control.SetChatFocus(ctx, "chat", foreign)
	require.Error(t, direct)
	refused, ok := errors.AsType[*connect.Error](err)
	require.True(t, ok)
	assert.Equal(t, direct.Error(), refused.Message())
}

func assertChatState(t *testing.T, chat coagentv1connect.ChatServiceClient, control controllerapi.ChatFocusController,
	id, cursor int64, entered bool,
) {
	t.Helper()
	focus, err := chat.GetChatFocus(t.Context(), &coagentv1.GetChatFocusRequest{ChatId: "chat"})
	require.NoError(t, err)
	assert.Equal(t, id, focus.GetSessionId())
	direct, err := control.ChatFocus(t.Context(), "chat")
	require.NoError(t, err)
	assert.Equal(t, direct, focus.GetSessionId())
	state, err := chat.GetChatCursor(t.Context(), &coagentv1.GetChatCursorRequest{ChatId: "chat"})
	require.NoError(t, err)
	assert.Equal(t, id, state.GetSessionId())
	assert.Equal(t, cursor, state.GetCursor())
	assert.Equal(t, entered, state.GetEntered())
	directID, directCursor, directEntered, err := control.ChatCursor(t.Context(), "chat")
	require.NoError(t, err)
	assert.Equal(t, directID, state.GetSessionId())
	assert.Equal(t, directCursor, state.GetCursor())
	assert.Equal(t, directEntered, state.GetEntered())
}
