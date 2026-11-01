package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/managerdelivery"
	"github.com/pilat/coagent/internal/registry"
)

var (
	_ managerdelivery.ChatPort      = relayChatPort{}
	_ managerdelivery.ChatTransport = relayChatTransport{}
)

type relayChatPort struct {
	controllerapi.ChatFocusController
	chat string
}

type relayChatTransport struct{ sent chan string }

func TestHarnessScenario_AssistantRelaysWorkAnswer(t *testing.T) {
	assistantRelayScenario(t, "work answer", "introduced")
}

func TestHarnessScenario_AssistantFramesQuestionAfterRelay(t *testing.T) {
	assistantRelayScenario(t, "Apply the proposed fix?", "Apply the fix now?")
}

func assistantRelayScenario(t *testing.T, answer, spoken string) {
	t.Helper()
	release := make(chan struct{})
	t.Cleanup(func() { closeOnce(release) })
	h := newHarness(t, harnessOptions{respond: func(system string, messages []llmwire.Message) *llmwire.Response {
		if !strings.HasPrefix(system, registry.AssistantPrompt) {
			<-release
			return textReply(answer)
		}
		if hasUserContaining(messages, "answered:") {
			return textReply(spoken)
		}
		if hasToolResultFor(messages, "open") {
			return textReply("started")
		}
		return callReply("open", "open", `{"project":"deskwork","about":"check","text":"check this"}`)
	}})
	root, err := h.mgr.EnsureAssistantRoot(h.ctx, scenarioManagerID)
	require.NoError(t, err)
	controller := newTestController(h.mgr, h.mgr.build.Config, nil, nil).ForManager(scenarioManagerID)
	chat, ok := controller.(controllerapi.ChatFocusController)
	require.True(t, ok)
	require.NoError(t, chat.BindChat(h.ctx, "chat"))
	h.startInboxWake()
	require.NoError(t, h.mgr.sendToSession(h.ctx, root.ID, "please check"))
	h.waitUntil("assistant started work", func() bool {
		return assistantSayCount(t, h, root.ID, "started") == 1 && !h.mgr.HasActiveLoop(root.ID)
	})
	closeOnce(release)
	h.waitUntil("assistant introduced result", func() bool {
		return assistantSayCount(t, h, root.ID, spoken) == 1 && !h.mgr.HasActiveLoop(root.ID)
	})
	rows, err := chat.ChatOutputs(h.ctx, "chat", 0, 20)
	require.NoError(t, err)
	var messages []controllerapi.ChatOutput
	for _, row := range rows {
		if row.Type == "message" {
			messages = append(messages, row)
		}
	}
	require.Len(t, messages, 3)
	assert.Equal(t, "started", messages[0].Content)
	assert.NotZero(t, messages[1].Origin)
	assert.Equal(t, "deskwork · check", messages[1].OriginName)
	assert.Equal(t, spoken, messages[2].Content)
	assert.Zero(t, messages[2].Origin)
	var original string
	require.NoError(t, h.db.QueryRowContext(h.ctx, `SELECT content FROM session_outbox
		WHERE session_id = ? AND source_key LIKE 'message:%:final'`, messages[1].Origin).Scan(&original))
	assert.Equal(t, original, messages[1].Content)
	sent := relayChatDelivery(t, h, chat, root.ID, messages[2].ID)
	assert.Equal(
		t,
		[]string{"started", fmt.Sprintf("#%d deskwork · check\n%s", messages[1].Origin, original), spoken},
		sent,
	)
	seen, err := h.store.ChatSeen(h.ctx, scenarioManagerID, "chat", messages[1].Origin)
	require.NoError(t, err)
	assert.Zero(t, seen)
}

func relayChatDelivery(t *testing.T, h *harness, chat controllerapi.ChatFocusController, root, last int64) []string {
	t.Helper()
	transport := relayChatTransport{sent: make(chan string, 10)}
	loop := managerdelivery.NewChat(relayChatPort{ChatFocusController: chat, chat: "chat"}, transport)
	loop.Start(t.Context())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		require.NoError(t, loop.Stop(ctx))
	})
	h.waitUntil("chat consumed assistant outbox", func() bool {
		seen, err := h.store.ChatSeen(h.ctx, scenarioManagerID, "chat", root)
		return err == nil && seen == last
	})
	require.NoError(t, loop.Stop(t.Context()))
	close(transport.sent)
	var sent []string
	for text := range transport.sent {
		sent = append(sent, text)
	}
	return sent
}

func (p relayChatPort) Focus(ctx context.Context) (int64, error) { return p.ChatFocus(ctx, p.chat) }
func (p relayChatPort) ResetFocus(ctx context.Context, id int64) (bool, error) {
	return p.ResetChatFocus(ctx, p.chat, id)
}

func (p relayChatPort) MarkSeen(ctx context.Context, id, output int64) error {
	return p.MarkChatSeen(ctx, p.chat, id, output)
}

func (p relayChatPort) Killed(ctx context.Context, id int64) (bool, error) {
	return p.SessionKilled(ctx, id)
}

func (p relayChatPort) Outputs(ctx context.Context, id int64, limit int) ([]managerdelivery.ChatRow, error) {
	rows, err := p.ChatOutputs(ctx, p.chat, id, limit)
	outputs := make([]managerdelivery.ChatRow, 0, len(rows))
	for _, row := range rows {
		outputs = append(outputs, relayDeliveryRow(row))
	}
	return outputs, err
}

func (p relayChatPort) LastSeen(ctx context.Context, id int64) (managerdelivery.ChatRow, bool, error) {
	row, found, err := p.LastSeenChatOutput(ctx, p.chat, id)
	return relayDeliveryRow(row), found, err
}

func (p relayChatPort) Sessions(ctx context.Context) ([]managerdelivery.ChatSession, error) {
	rows, err := p.ChatSessions(ctx, p.chat)
	outputs := make([]managerdelivery.ChatSession, 0, len(rows))
	for _, row := range rows {
		outputs = append(outputs, managerdelivery.ChatSession{
			ID: row.ID, Name: row.Name, Unread: row.Unread,
			LatestUnread: row.LatestUnread, LatestUnreadID: row.LatestUnreadID, Scheduled: row.Scheduled,
		})
	}
	return outputs, err
}

func (p relayChatTransport) Send(ctx context.Context, text string) managerdelivery.SendResult {
	select {
	case p.sent <- text:
		return managerdelivery.SendResult{}
	case <-ctx.Done():
		return managerdelivery.SendResult{Class: managerdelivery.SendTransient}
	}
}

func relayDeliveryRow(row controllerapi.ChatOutput) managerdelivery.ChatRow {
	return managerdelivery.ChatRow{
		ID: row.ID, Name: row.Name, Type: row.Type, Content: row.Content,
		Origin: row.Origin, OriginName: row.OriginName,
	}
}
