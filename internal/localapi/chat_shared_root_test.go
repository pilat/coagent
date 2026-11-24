package localapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
	"github.com/pilat/coagent/internal/controllerapi"
)

func TestChatService_TwoClientsFocusedOnOneRootEachAdvanceTheirOwnCursor(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	ctx := t.Context()
	id := inProcessRoot(t, f, "alpha")
	alpha := coagentv1connect.NewChatServiceClient(inProcessClient(t, f, "alpha"))
	beta := coagentv1connect.NewChatServiceClient(inProcessClient(t, f, "beta"))
	entry, err := coagentv1connect.NewOutputServiceClient(inProcessClient(t, f, "alpha")).ReadEntry(
		ctx, &coagentv1.ReadEntryRequest{SessionId: id, Limit: 3},
	)
	require.NoError(t, err)
	for _, chat := range []coagentv1connect.ChatServiceClient{alpha, beta} {
		_, err := chat.BindChat(ctx, &coagentv1.BindChatRequest{ChatId: "c"})
		require.NoError(t, err)
		_, err = chat.SetChatFocus(ctx, &coagentv1.SetChatFocusRequest{ChatId: "c", SessionId: id})
		require.NoError(t, err)
		entered, err := chat.EnterChat(ctx, &coagentv1.EnterChatRequest{
			ChatId: "c", SessionId: id, NewestId: entry.GetNewestId(),
		})
		require.NoError(t, err)
		require.True(t, entered.GetEntered())
	}
	rows := []int64{f.append(t, id, "r1"), f.append(t, id, "r2"), f.append(t, id, "r3")}
	for i, chat := range []coagentv1connect.ChatServiceClient{alpha, beta} {
		for _, row := range rows {
			for _, expected := range []bool{true, false} {
				response, err := chat.AdvanceChat(ctx, &coagentv1.AdvanceChatRequest{
					ChatId: "c", SessionId: id, OutputId: row,
				})
				require.NoError(t, err)
				assert.Equal(t, expected, response.GetAdvanced())
			}
			stale, err := chat.AdvanceChat(ctx, &coagentv1.AdvanceChatRequest{
				ChatId: "c", SessionId: id, OutputId: entry.GetNewestId(),
			})
			require.NoError(t, err)
			assert.False(t, stale.GetAdvanced())
			other, want := beta, entry.GetNewestId()
			if i == 1 {
				other, want = alpha, rows[len(rows)-1]
			}
			cursor, err := other.GetChatCursor(ctx, &coagentv1.GetChatCursorRequest{ChatId: "c"})
			require.NoError(t, err)
			assert.Equal(t, want, cursor.GetCursor())
		}
	}
	for owner, chat := range map[string]coagentv1connect.ChatServiceClient{"alpha": alpha, "beta": beta} {
		control := f.factory.ForManager(owner).(controllerapi.ChatFocusController)
		focus, cursor, entered, err := control.ChatCursor(ctx, "c")
		require.NoError(t, err)
		assert.Equal(t, id, focus)
		assert.Equal(t, rows[len(rows)-1], cursor)
		assert.True(t, entered)
		state, err := chat.GetChatCursor(ctx, &coagentv1.GetChatCursorRequest{ChatId: "c"})
		require.NoError(t, err)
		assert.Equal(t, cursor, state.GetCursor())
	}
}
