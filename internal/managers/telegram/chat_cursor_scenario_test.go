package telegram

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/sessionstore"
)

func TestHarnessScenario_SwitchSendsHeaderLastThreeThenLive(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	id := createChatWork(t, h)
	for row := 1; row <= 5; row++ {
		enqueueChatMessage(t, h, id, fmt.Sprint(row))
	}
	_, err := h.sessions.EnqueueOutput(t.Context(), sessionstore.OutputDraft{
		SessionID: id, Type: sessionstore.OutputSessionOpened,
		Attributes: map[string]any{"name": "project", "work_dir": h.workDir},
	})
	require.NoError(t, err)
	m := newChatTelegramManager(t, h)
	require.NoError(t, m.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, m.Stop(context.Background())) })
	m.processUpdate(t.Context(), chatTelegramUpdate(fmt.Sprintf("/switch %d", id)))
	awaitChatTail(t, h, id)
	want := []string{fmt.Sprintf("<b>→ #%d project</b>", id)}
	for row := 3; row <= 5; row++ {
		want = append(want, fmt.Sprintf("<b>#%d project</b>\n\n%d", id, row))
	}
	assert.Equal(t, want, chatMessages(h))
	enqueueChatMessage(t, h, id, "6")
	m.wakeDelivery()
	awaitChatTail(t, h, id)
	want = append(want, fmt.Sprintf("<b>#%d project</b>\n\n6", id))
	assert.Equal(t, want, chatMessages(h))
	m.processUpdate(t.Context(), chatTelegramUpdate(fmt.Sprintf("/switch %d", id)))
	awaitChatTail(t, h, id)
	want = append(want, fmt.Sprintf("<b>→ #%d project</b>", id))
	for row := 4; row <= 6; row++ {
		want = append(want, fmt.Sprintf("<b>#%d project</b>\n\n%d", id, row))
	}
	assert.Equal(t, want, chatMessages(h))
}

func TestHarnessScenario_EntryWithFewerThanThreeRows(t *testing.T) {
	for _, count := range []int{0, 1} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			h := newDelayedTelegramHarness(t)
			id := createChatWork(t, h)
			if count == 1 {
				enqueueChatMessage(t, h, id, "only row")
			}
			m := newChatTelegramManager(t, h)
			require.NoError(t, m.Start(t.Context()))
			t.Cleanup(func() { require.NoError(t, m.Stop(context.Background())) })
			m.processUpdate(t.Context(), chatTelegramUpdate(fmt.Sprintf("/switch %d", id)))
			awaitChatTail(t, h, id)
			want := []string{fmt.Sprintf("<b>→ #%d project</b>", id)}
			if count == 1 {
				want = append(want, fmt.Sprintf("<b>#%d project</b>\n\nonly row", id))
			}
			assert.Equal(t, want, chatMessages(h))
		})
	}
}

func TestHarnessScenario_RestartAfterSendResendsNothing(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	id := createChatWork(t, h)
	first := newChatTelegramManager(t, h)
	require.NoError(t, first.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, first.Stop(context.Background())) })
	first.processUpdate(t.Context(), chatTelegramUpdate(fmt.Sprintf("/switch %d", id)))
	awaitChatTail(t, h, id)
	enqueueChatMessage(t, h, id, "before restart")
	first.wakeDelivery()
	awaitChatTail(t, h, id)
	require.NoError(t, first.Stop(context.Background()))
	second := newChatTelegramManager(t, h)
	require.NoError(t, second.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, second.Stop(context.Background())) })
	enqueueChatMessage(t, h, id, "after restart")
	second.wakeDelivery()
	awaitChatTail(t, h, id)
	assert.Equal(t, []string{
		fmt.Sprintf("<b>→ #%d project</b>", id),
		fmt.Sprintf("<b>#%d project</b>\n\nbefore restart", id),
		fmt.Sprintf("<b>#%d project</b>\n\nafter restart", id),
	}, chatMessages(h))
}

func TestHarnessScenario_ClearEntersSuccessor(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	require.NoError(t, h.service.Start(t.Context()))
	old := createChatWork(t, h)
	m := newChatTelegramManager(t, h)
	require.NoError(t, m.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, m.Stop(context.Background())) })
	m.processUpdate(t.Context(), chatTelegramUpdate(fmt.Sprintf("/switch %d", old)))
	awaitChatTail(t, h, old)
	before := len(chatMessages(h))
	m.processUpdate(t.Context(), chatTelegramUpdate("/clear"))
	chat := h.controller.(controllerapi.ChatFocusController)
	key := strconv.FormatInt(harnessChatID, 10)
	var successor int64
	require.Eventually(t, func() bool {
		focus, err := chat.ChatFocus(t.Context(), key)
		successor = focus
		return err == nil && focus != 0 && focus != old
	}, 5*time.Second, time.Millisecond)
	awaitChatTail(t, h, successor)
	require.NoError(t, h.sessions.WithTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `INSERT INTO session_outbox
			(session_id,type,content,attributes,created_at) VALUES (?, 'message', 'late old output', '{}', ?)`,
			old, time.Now().UTC())
		return err
	}))
	enqueueChatMessage(t, h, successor, "successor barrier")
	m.wakeDelivery()
	awaitChatTail(t, h, successor)
	assert.Equal(t, []string{
		fmt.Sprintf("<b>→ #%d project</b>", successor),
		fmt.Sprintf("<b>#%d project</b>\n\nSession cleared.", successor),
		fmt.Sprintf("<b>#%d project</b>\n\nsuccessor barrier", successor),
	}, chatMessages(h)[before:])
}

func TestHarnessScenario_SessionsActivityOrder(t *testing.T) {
	h := newDelayedTelegramHarness(t)
	var ids []int64
	for range 31 {
		ids = append(ids, createChatWork(t, h))
	}
	base := time.Date(2026, time.November, 4, 10, 0, 0, 0, time.UTC)
	require.NoError(t, h.sessions.WithTx(t.Context(), func(tx *sql.Tx) error {
		for i, id := range ids {
			_, err := tx.ExecContext(t.Context(), "UPDATE session_outbox SET created_at = ? WHERE session_id = ?",
				base.Add(time.Duration(i)*time.Hour), id)
			if err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(
			t.Context(),
			"INSERT INTO schedules(session_id,cron_expr,input_message) VALUES (?, '0 9 * * *','')",
			ids[30],
		)
		return err
	}))
	// The oldest-created root has the newest activity.
	last := enqueueChatMessage(t, h, ids[0], "newest activity")
	require.NoError(t, h.sessions.WithTx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(
			t.Context(),
			"UPDATE session_outbox SET created_at = ? WHERE id = ?",
			base.Add(32*time.Hour),
			last,
		)
		return err
	}))
	chat := h.controller.(controllerapi.ChatFocusController)
	require.NoError(t, chat.SetChatFocus(t.Context(), strconv.FormatInt(harnessChatID, 10), ids[0]))
	m := newChatTelegramManager(t, h)
	require.NoError(t, m.Start(t.Context()))
	t.Cleanup(func() { require.NoError(t, m.Stop(context.Background())) })
	awaitChatTail(t, h, ids[0])
	before := len(chatMessages(h))
	m.processUpdate(t.Context(), chatTelegramUpdate("/sessions"))
	messages := chatMessages(h)[before:]
	require.Len(t, messages, 1)
	lines := strings.Split(messages[0], "\n")
	want := []string{fmt.Sprintf("#%d project ◀", ids[0]), fmt.Sprintf("#%d project ⏰", ids[30])}
	for i := 29; i >= 2; i-- {
		want = append(want, fmt.Sprintf("#%d project", ids[i]))
	}
	want = append(want, "… and 1 more")
	assert.Equal(t, want, lines)
	assert.NotContains(t, messages[0], " new")
}
