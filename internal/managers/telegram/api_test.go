package telegram

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
)

func TestRefusalText_ReplyIsTheControllerTextByteForByte(t *testing.T) {
	t.Parallel()
	fake := &fakeController{createSessionErr: errors.New("line one\nline two")}
	var messages []string
	manager := newCmdTestManager(t, fake, &messages)
	manager.handleChatText(t.Context(), "/spawn /x")
	assert.Equal(t, []string{"line one line two"}, messages)
}

func TestChatPort_MapsEveryOutputType(t *testing.T) {
	t.Parallel()
	fake := &fakeController{outputs: map[int64][]controllerapi.ChatOutput{
		42: {
			{ID: 1, Type: controllerapi.OutputMessage},
			{ID: 2, Type: controllerapi.OutputSessionOpened},
			{ID: 3, Type: controllerapi.OutputSessionReplaced},
			{ID: 4, Type: controllerapi.OutputSessionClosed},
			{ID: 5, Type: "other"},
		},
	}}
	port := &chatPort{api: servedAPI(t, fake, fake), chatID: "chat"}
	rows, err := port.Outputs(t.Context(), 42, 0, 100)
	require.NoError(t, err)
	require.Len(t, rows, 5)
	var types []string
	for _, row := range rows {
		types = append(types, row.Type)
	}
	assert.Equal(t, []string{"message", "session_opened", "session_replaced", "session_closed", ""}, types)
}
