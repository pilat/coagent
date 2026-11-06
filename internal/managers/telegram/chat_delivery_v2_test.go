package telegram

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
)

func TestSessionsLimitAndNumberedControlBeyondLimit(t *testing.T) {
	for _, total := range []int{29, 30, 35} {
		t.Run(strconv.Itoa(total), func(t *testing.T) {
			t.Parallel()
			ctrl := &fakeController{}
			for id := 1; id <= total; id++ {
				ctrl.listSessions = append(ctrl.listSessions, controllerapi.SessionInfo{
					ID: int64(id), Name: fmt.Sprintf("work %d", id),
					Attributes: map[string]any{"manager_id": "telegram-main"},
				})
			}
			var messages []string
			manager := newCmdTestManager(t, ctrl, &messages)
			manager.handleChatText(t.Context(), "/sessions")
			require.Len(t, messages, 1)
			lines := strings.Split(messages[0], "\n")
			assert.Equal(t, "#1 work 1", lines[0])
			if total <= 30 {
				assert.Len(t, lines, total)
				assert.NotContains(t, messages[0], "more —")
				return
			}
			require.Len(t, lines, 31)
			assert.Equal(t, "#30 work 30", lines[29])
			assert.Equal(t, "… and 5 more", lines[30])
			manager.handleChatText(t.Context(), "/kill 34")
			assert.Equal(t, []controllerapi.SessionMessageData{{SessionID: 34, Message: "/kill"}}, ctrl.messageCalls)
			assert.Zero(t, ctrl.focus)
		})
	}
}
