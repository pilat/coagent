package telegram

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/managerdelivery"
)

const replayManagerID = "telegram:main"

func TestHarnessScenario_DaemonTraceDeliversThroughDurableOutbox(t *testing.T) {
	names := daemonTraceNames(t)

	got := telegramRenderedFile{}
	for _, name := range names {
		trace := readDaemonTrace(t, name)
		calls := replayDaemonTraceThroughOutbox(t, trace)

		got.Scenarios = append(got.Scenarios, telegramRenderedScenario{
			Trace:      name,
			SourceTest: trace.SourceTest,
			Calls:      calls,
		})
	}

	if *updateHarnessTraces {
		writeRenderedGolden(t, got)

		return
	}

	want := readRenderedGolden(t)
	require.Len(t, got.Scenarios, len(want.Scenarios),
		"every recorded daemon trace must have a rendered golden; regenerate with -update-traces")

	for i, scenario := range got.Scenarios {
		t.Run(scenario.Trace, func(t *testing.T) {
			assert.Equal(t, want.Scenarios[i], scenario, "Telegram traffic delivered from the durable outbox")
		})
	}
}

func TestHarnessScenario_SetManagerTracesProduceNoTelegramTraffic(t *testing.T) {
	for _, name := range []string{
		"set_manager_patch_restart.json",
		"set_manager_reapply_restart.json",
	} {
		t.Run(name, func(t *testing.T) {
			trace := readDaemonTrace(t, name)
			calls := replayDaemonTraceThroughOutbox(t, trace)
			assert.Empty(t, calls, "a CLI-owned trace must produce no Telegram traffic")
		})
	}
}

func TestHarnessScenario_NarratedTraceReplaysAllNarrationsAndFinal(t *testing.T) {
	calls := replayDaemonTraceThroughOutbox(t, readDaemonTrace(t, "output_chain_narrated_tools.json"))
	var messages []string

	for _, call := range calls {
		messages = append(messages, call.Text)
	}

	assert.Equal(t, []string{
		"→ #1 &lt;name&gt;",
		"#1 &lt;name&gt;\nReading the repo\n\n→ ls: <code>.</code>",
		"#1 &lt;name&gt;\nUpdating the task list\n\n→ todowrite", "#1 &lt;name&gt;\nAll done.",
	}, messages)
}

func TestHarnessScenario_OutputsRetainOwnershipWithoutCreatedNotification(t *testing.T) {
	calls := replayDaemonTraceThroughOutbox(t, readDaemonTrace(t, "stop_interrupted_chain.json"))
	var messages []string

	for _, call := range calls {
		messages = append(messages, call.Text)
	}

	assert.Equal(t, []string{
		"→ #1 &lt;name&gt;",
		"#1 &lt;name&gt;\n⏳ Stopping…", "#1 &lt;name&gt;\n⏸️ Session stopped\nCancelled background processes: 0",
	}, messages)
}

func TestHarnessScenario_ReplaySelectsFirstOwnedMessageAndSkipsOtherTails(t *testing.T) {
	trace := harnessTraceFile{
		Owners: map[int64]string{1: "foreign", 2: replayManagerID, 3: replayManagerID},
		Outputs: []harnessTraceOutput{
			{ID: 1, SessionID: 1, Type: controllerapi.OutputMessage, Content: "foreign"},
			{ID: 2, SessionID: 3, Type: controllerapi.OutputSessionOpened},
			{ID: 3, SessionID: 2, Type: controllerapi.OutputMessage, Content: "selected first"},
			{ID: 4, SessionID: 3, Type: controllerapi.OutputMessage, Content: "other output"},
			{ID: 5, SessionID: 2, Type: controllerapi.OutputMessage, Content: "selected second"},
		},
	}
	calls := replayDaemonTraceThroughOutbox(t, trace)
	require.Len(t, calls, 3)
	assert.Equal(t, "→ #2 &lt;name&gt;", calls[0].Text)
	assert.Equal(t, "#2 &lt;name&gt;\nselected first", calls[1].Text)
	assert.Equal(t, "#2 &lt;name&gt;\nselected second", calls[2].Text)
	controller := &fakeController{managerID: replayManagerID, listSessions: []controllerapi.SessionInfo{
		{ID: 1, Attributes: map[string]any{"manager_id": "foreign"}},
	}}
	require.EqualError(t, controller.SetChatFocus(t.Context(), "chat", 1), "#1 is not a live session")
}

func replayDaemonTraceThroughOutbox(t *testing.T, trace harnessTraceFile) []telegramHarnessCall {
	t.Helper()
	controller := &fakeController{managerID: replayManagerID, outputs: make(map[int64][]controllerapi.ChatOutput)}
	for id, owner := range trace.Owners {
		controller.listSessions = append(controller.listSessions, controllerapi.SessionInfo{
			ID: id, Name: "<name>", Attributes: map[string]any{"manager_id": owner},
		})
	}
	for _, output := range trace.Outputs {
		controller.outputs[output.SessionID] = append(controller.outputs[output.SessionID], controllerapi.ChatOutput{
			ID: output.ID, SessionID: output.SessionID, Type: output.Type, Content: output.Content,
		})
	}
	for _, row := range trace.Outputs {
		if row.Type == controllerapi.OutputMessage && trace.Owners[row.SessionID] == replayManagerID {
			require.NoError(t, controller.SetChatFocus(t.Context(), "chat", row.SessionID))
			break
		}
	}

	manager, err := New(config.ManagerEntry{
		ID: replayManagerID, BotToken: "test-token", AllowedUserIDs: []int64{harnessChatID},
	}, nil, controller)
	require.NoError(t, err)
	recorder := &delayedTelegramRecorder{}
	manager.httpClient = &http.Client{Transport: recorder}
	loop := managerdelivery.NewChat(
		&chatPort{controller: controller, chatID: "chat"},
		&outputTransport{manager: manager},
	)
	loop.Start(t.Context())
	require.Eventually(t, func() bool {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		rows := controller.outputs[controller.focus]
		return controller.focus == 0 ||
			(controller.entered && (len(rows) == 0 || controller.cursor == rows[len(rows)-1].ID))
	}, time.Second, time.Millisecond)
	require.NoError(t, loop.Stop(context.Background()))

	var calls []telegramHarnessCall
	for _, call := range recorder.snapshot() {
		calls = append(calls, telegramHarnessCall{
			Method: call.Method, ChatID: call.ChatID, Text: call.Text, ParseMode: call.ParseMode,
		})
	}
	return calls
}
