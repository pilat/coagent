package telegram

import (
	"context"
	"database/sql"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHarnessScenario_StartupBindsChatAndRestartKeepsSelection(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	key := strconv.FormatInt(harnessChatID, 10)
	manager := newAssistantTelegramManager(t, h)
	require.NoError(t, manager.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, manager.Stop(context.Background())) })
	var selected sql.NullInt64
	require.NoError(t, h.sessions.WithTx(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT session_id FROM chat_focus
			WHERE manager_id = ? AND chat_id = ?`, delayedTelegramManagerID, key).Scan(&selected)
	}))
	assert.False(t, selected.Valid)
	work := createChatWork(t, h)
	require.NoError(t, h.sessions.SetChatFocus(t.Context(), delayedTelegramManagerID, key, work))
	require.NoError(t, manager.Stop(context.Background()))
	restarted := newAssistantTelegramManager(t, h)
	require.NoError(t, restarted.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, restarted.Stop(context.Background())) })
	focus, err := h.sessions.ChatFocus(t.Context(), delayedTelegramManagerID, key)
	require.NoError(t, err)
	assert.Equal(t, work, focus)
}
