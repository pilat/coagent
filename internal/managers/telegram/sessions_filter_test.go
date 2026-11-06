package telegram

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
)

func TestSessionsWordFilter(t *testing.T) {
	for _, tt := range []struct {
		name    string
		command string
		want    string
	}{
		{"case insensitive", "/sessions ALpHa", "#1 Alpha project · first ◀\n#3 tools · alpha task"},
		{"trimmed phrase", "/sessions   PROJECT · FIRST   ", "#1 Alpha project · first ◀"},
		{"internal spaces preserved", "/sessions Alpha  project", "No sessions match \"Alpha  project\"."},
		{"no match", "/sessions Missing", "No sessions match \"Missing\"."},
		{"blank filter", "/sessions   ", "#1 Alpha project · first ◀\n#2 beta\n#3 tools · alpha task"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ctrl := &fakeController{focus: 1}
			for i, name := range []string{"Alpha project · first", "beta", "tools · alpha task"} {
				ctrl.listSessions = append(ctrl.listSessions, controllerapi.SessionInfo{
					ID: int64(i + 1), Name: name, Attributes: map[string]any{"manager_id": "telegram-main"},
				})
			}
			var messages []string
			manager := newCmdTestManager(t, ctrl, &messages)
			manager.handleChatText(t.Context(), tt.command)
			assert.Equal(t, []string{tt.want}, messages)
			assert.Empty(t, ctrl.messageCalls)
		})
	}
}

func TestHarnessScenario_SessionsFilterKeepsMarkers(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	one, two := createChatWork(t, h), createChatWork(t, h)
	require.NoError(t, h.sessions.SetSessionTitle(t.Context(), delayedTelegramManagerID, one, "Daily Check"))
	require.NoError(t, h.sessions.SetSessionTitle(t.Context(), delayedTelegramManagerID, two, "other work"))
	require.NoError(t, h.sessions.WithTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(),
			`INSERT INTO schedules(session_id,cron_expr,input_message) VALUES (?, '0 9 * * *','')`, one)
		return err
	}))
	chat := h.controller.(controllerapi.ChatFocusController)
	require.NoError(t, chat.SetChatFocus(t.Context(), strconv.FormatInt(harnessChatID, 10), one))
	manager := newChatTelegramManager(t, h)
	manager.processUpdate(t.Context(), chatTelegramUpdate("/sessions   DAILY CHECK   "))
	manager.processUpdate(t.Context(), chatTelegramUpdate("/sessions absent"))
	assert.Equal(t, []string{
		fmt.Sprintf("#%d project · Daily Check ⏰ ◀", one),
		"No sessions match \"absent\".",
	}, chatMessages(h))
}

func TestSessionsFilterLimit(t *testing.T) {
	for _, total := range []int{0, 29, 30, 35} {
		t.Run(fmt.Sprint(total), func(t *testing.T) {
			t.Parallel()
			ctrl := &fakeController{}
			for i := 1; i <= 40+total; i++ {
				name := "other"
				if i > 40 {
					name = fmt.Sprintf("match %d", i-40)
				}
				ctrl.listSessions = append(ctrl.listSessions, controllerapi.SessionInfo{
					ID: int64(i), Name: name, Attributes: map[string]any{"manager_id": "telegram-main"},
				})
			}
			var messages []string
			manager := newCmdTestManager(t, ctrl, &messages)
			manager.handleChatText(t.Context(), "/sessions match")
			require.Len(t, messages, 1)
			want := make([]string, 0, 31)
			for i := 1; i <= min(total, 30); i++ {
				want = append(want, fmt.Sprintf("#%d match %d", 40+i, i))
			}
			if total == 0 {
				want = append(want, "No sessions match \"match\".")
			}
			if total > 30 {
				want = append(want, "… and 5 more")
			}
			assert.Equal(t, strings.Join(want, "\n"), messages[0])
		})
	}
}
