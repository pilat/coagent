package telegram

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/pilat/coagent/internal/controllerapi"
)

func TestNumberedCommandsAcceptOneHash(t *testing.T) {
	for _, prefix := range []string{"", "#"} {
		t.Run("prefix "+prefix, func(t *testing.T) {
			t.Parallel()
			var messages []string
			ctrl := &fakeController{focus: 42, listSessions: []controllerapi.SessionInfo{
				{ID: 12, Name: "work", Attributes: map[string]any{"manager_id": "telegram-main"}},
			}}
			manager := newCmdTestManager(t, ctrl, &messages)
			manager.handleChatText(t.Context(), "/kill "+prefix+"12")
			manager.handleChatText(t.Context(), "/stop "+prefix+"12")
			assert.Equal(t, []controllerapi.SessionMessageData{
				{SessionID: 12, Message: "/kill"}, {SessionID: 12, Message: "/stop"},
			}, ctrl.messageCalls)
			assert.Equal(t, int64(42), ctrl.focus)
			manager.handleChatText(t.Context(), "/switch "+prefix+"12")
			assert.Equal(t, int64(12), ctrl.focus)
			assert.Empty(t, messages)
		})
	}
}

func TestNumberedCommandsRejectMalformedHash(t *testing.T) {
	for _, command := range []string{"/switch", "/kill", "/stop"} {
		for _, ref := range []string{"#", "##12"} {
			t.Run(command+ref, func(t *testing.T) {
				t.Parallel()
				var messages []string
				ctrl := &fakeController{focus: 42}
				manager := newCmdTestManager(t, ctrl, &messages)
				manager.handleChatText(t.Context(), command+" "+ref)
				assert.Equal(t, []string{"Usage: " + command + " N."}, messages)
				assert.Empty(t, ctrl.messageCalls)
				assert.Equal(t, int64(42), ctrl.focus)
			})
		}
	}
}
