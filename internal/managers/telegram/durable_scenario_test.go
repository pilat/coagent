package telegram

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/controllerapi"
	"github.com/pilat/coagent/internal/managerdelivery"
	"github.com/pilat/coagent/internal/migrate"
	"github.com/pilat/coagent/internal/sessionevent"
	"github.com/pilat/coagent/internal/sessionstore"
)

// The daemon traces are transport-neutral: this half converts each recorded
// event into its durable outbox equivalent and drains the queue through the
// production output transport, so the rendered golden asserts exact
// send/edit/delete traffic from the delivery contract itself.
//
// Traces recorded for another manager's conversation must produce no Telegram
// traffic at all — ownership is part of every outbox row.
const replayManagerID = "telegram-main"

func TestHarnessScenario_DaemonTraceDeliversThroughDurableOutbox(t *testing.T) {
	names := daemonTraceNames(t)

	got := telegramRenderedFile{}
	for _, name := range names {
		trace := readDaemonTrace(t, name)
		calls := replayDaemonTraceThroughOutbox(t, trace)
		if len(trace.Claims) > 0 {
			calls = replayProductionClaims(t, trace)
		}

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

// A separately recorded guard: regenerating goldens must never hide an
// ownership regression where another manager's backlog renders here.
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

// A durable manager renders nothing directly from observer notifications: they
// are wake hints at most.
func TestHarnessScenario_DurableTelegramIgnoresObserverNotifications(t *testing.T) {
	var calls []telegramHarnessCall

	queue := &stubDeliveryQueue{}
	transport := &stubDeliveryTransport{t: t}
	manager := newTelegramHarnessManager(t, &fakeController{}, &calls)
	manager.delivery = managerdelivery.New(queue, transport)

	manager.handleNotification(t.Context(), controllerapi.SessionNotification{
		SessionID: harnessSessionID,
		Notification: sessionevent.Notification{
			Type: sessionevent.NotifyMessage, Message: "✅ observer answer",
		},
	})

	assert.Empty(t, calls, "observer notifications must not render directly")
	assert.Zero(t, transport.delivered, "ordinary output reaches managers only through the outbox")
}

type stubDeliveryQueue struct{}

func (q *stubDeliveryQueue) Claim(context.Context) (*managerdelivery.Item, error) {
	return nil, managerdelivery.ErrNoItem
}

func (q *stubDeliveryQueue) Ack(context.Context, *managerdelivery.Item, managerdelivery.Result) error {
	return nil
}

func (q *stubDeliveryQueue) Retry(context.Context, *managerdelivery.Item, string, time.Time) error {
	return nil
}

func (q *stubDeliveryQueue) Block(context.Context, *managerdelivery.Item, string) error { return nil }

type stubDeliveryTransport struct {
	t         *testing.T
	delivered int
}

func (s *stubDeliveryTransport) Deliver(_ context.Context, _ *managerdelivery.Item) managerdelivery.Result {
	s.delivered++

	return managerdelivery.Result{}
}

func traceOwner(trace harnessTraceFile) string {
	for _, event := range trace.Trace {
		if owner, ok := event.Attributes[controllerapi.SessionAttributeManagerID].(string); ok && owner != "" {
			return owner
		}
	}

	return ""
}

// replayDaemonTraceThroughOutbox converts one recorded trace into outbox rows
// and drains them through the production output transport.
func replayDaemonTraceThroughOutbox(t *testing.T, trace harnessTraceFile) []telegramHarnessCall {
	t.Helper()

	if owner := traceOwner(trace); owner != "" && owner != replayManagerID {
		return nil
	}

	ctx := context.Background()
	store, rootID := newDurableReplayStore(t)

	for i, event := range trace.Trace {
		draft, ok := durableReplayDraft(t, rootID, i, event)
		if !ok {
			continue
		}

		_, err := store.EnqueueOutput(ctx, draft)
		require.NoError(t, err)
	}

	var calls []telegramHarnessCall
	manager := newTelegramHarnessManager(t, &fakeController{}, &calls)
	transport := &outputTransport{manager: manager}

	for {
		claim, err := store.ClaimOutputHead(ctx, replayManagerID)
		if errors.Is(err, sessionstore.ErrNoOutput) {
			break
		}

		require.NoError(t, err)

		data := &controllerapi.OutputClaimData{
			ID: claim.Output.ID, SessionID: claim.Output.SessionID,
			Type: string(claim.Output.Type), Content: claim.Output.Content,
			Attributes: claim.Output.Attributes, AttemptID: claim.Output.AttemptID,
			AttemptSeq:        claim.Output.AttemptSeq,
			SessionAttributes: claim.SessionAttributes,
		}
		if claim.PreviousDeliveredOutput != nil {
			data.PreviousMessageAttributes = claim.PreviousDeliveredOutput.Attributes
			data.PreviousMessageType = string(claim.PreviousDeliveredOutput.Type)
		}

		result := transport.Deliver(ctx, &managerdelivery.Item{
			ID: data.ID, AttemptID: data.AttemptID, Attempts: data.AttemptSeq, Payload: data,
		})
		require.Empty(t, result.Error, "durable replay must not fail delivery")

		require.NoError(t, store.AckOutput(
			ctx, replayManagerID, data.ID, data.AttemptID, result.MessageIDs, result.SessionPatch,
		))
	}

	return calls
}

// durableReplayDraft maps one recorded event onto its outbox equivalent.
// Lifecycle opens are already committed by root creation; state changes and
// input receipts have no user-visible output.
func durableReplayDraft(
	t *testing.T,
	rootID int64,
	index int,
	event harnessTraceEvent,
) (sessionstore.OutputDraft, bool) {
	t.Helper()

	switch event.Type {
	case "message":
		content := event.Message

		return sessionstore.OutputDraft{
			SessionID: rootID, Type: sessionstore.OutputMessagePersistent, Content: content,
			SourceKey:   fmt.Sprintf("trace:%d:message", index),
			Fingerprint: sessionstore.OutputFingerprint(sessionstore.OutputMessagePersistent, content, rootID, nil),
		}, true
	case "waiting":
		display, identity := replayWaitingItems(event.Waiting)
		content := sessionevent.FormatWaiting(harnessNotification(t, event).Waiting)
		attributes := map[string]any{"waiting": display, "waiting_identity": identity}

		return sessionstore.OutputDraft{
			SessionID: rootID, Type: sessionstore.OutputMessageReplaceable, Content: content,
			Attributes: attributes,
			SourceKey:  fmt.Sprintf("trace:%d:waiting", index),
			Fingerprint: sessionstore.OutputFingerprint(
				sessionstore.OutputMessageReplaceable, content, rootID, attributes,
			),
		}, true
	default:
		return sessionstore.OutputDraft{}, false
	}
}

func replayWaitingItems(items []harnessTraceWait) ([]map[string]any, []map[string]any) {
	wakeAt := time.Date(2026, time.January, 2, 15, 4, 0, 0, time.UTC)

	display := make([]map[string]any, 0, len(items))
	identity := make([]map[string]any, 0, len(items))

	for i, item := range items {
		switch sessionevent.WaitKind(item.Kind) {
		case sessionevent.WaitSleep:
			display = append(display, map[string]any{"wake_at": wakeAt.Format(time.RFC3339)})
			identity = append(identity, map[string]any{"tool_call_id": fmt.Sprintf("call-%d", i+1)})
		case sessionevent.WaitSubagent:
			display = append(display, map[string]any{"child_id": int64(i + 1)})
			identity = append(identity, map[string]any{
				"child_id": int64(i + 1), "activation_seq": int64(1),
			})
		}
	}

	return display, identity
}

func newDurableReplayStore(t *testing.T) (sessionstore.Store, int64) {
	t.Helper()

	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "replay.db")

	db, err := migrate.OpenDB(ctx, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, migrate.Run(ctx, db, dbPath))

	res, err := db.ExecContext(ctx, `INSERT INTO projects (work_dir, name) VALUES ('<workdir>', 'p')`)
	require.NoError(t, err)

	projectID, err := res.LastInsertId()
	require.NoError(t, err)

	store := sessionstore.NewStore(db)
	root, _, err := store.CreateManagerRoot(ctx, sessionstore.ManagerRootCreate{
		ProjectID: projectID, Model: "model",
		Attributes: map[string]any{controllerapi.SessionAttributeManagerID: replayManagerID},
		Name:       "<name>", WorkDir: "<workdir>",
	})
	require.NoError(t, err)
	require.NoError(t, store.BindManager(ctx, replayManagerID, "telegram", map[string]any{
		"bot_user_id": int64(42),
		"chat_id":     int64(harnessChatID),
		"topology":    "group",
	}))

	return store, root.ID
}

// replayProductionClaims feeds the recorded production OutputClaimData sequence
// straight through the production output transport. Nothing is reconstructed:
// the claims carry their insertion-time generations, so the transport itself
// makes every edit-versus-send decision the manager would have made live.
func replayProductionClaims(t *testing.T, trace harnessTraceFile) []telegramHarnessCall {
	t.Helper()

	if len(trace.Claims) == 0 {
		return nil
	}

	var calls []telegramHarnessCall
	manager := newTelegramHarnessManager(t, &fakeController{}, &calls)
	transport := &outputTransport{manager: manager}

	resolved := map[string][]string{}
	var lastDelivered []string

	// materialize maps one recorded receipt placeholder to real message ids:
	// the placeholder of the previous claim resolves to that claim's own
	// delivery result; anything unknown is created as a fresh real message so
	// edit targets always exist.
	materialize := func(placeholders []string) []string {
		ids := make([]string, 0, len(placeholders))
		for _, placeholder := range placeholders {
			if resolvedIDs, ok := resolved[placeholder]; ok {
				ids = append(ids, resolvedIDs...)

				continue
			}

			if lastDelivered != nil {
				resolved[placeholder] = lastDelivered
				ids = append(ids, lastDelivered...)

				continue
			}

			id, err := manager.sendMessageChunk(context.Background(), "recorded receipt", nil, harnessTopicID)
			require.NoError(t, err)
			resolved[placeholder] = []string{fmtRS(id)}
			ids = append(ids, fmtRS(id))
		}

		return ids
	}

	for _, claim := range trace.Claims {
		data := &controllerapi.OutputClaimData{
			Type:                         claim.Type,
			Content:                      claim.Content,
			Attributes:                   claim.Attributes,
			SourceKey:                    claim.SourceKey,
			ModelInputGeneration:         claim.ModelInputGeneration,
			PreviousMessageType:          claim.PreviousMessageType,
			PreviousModelInputGeneration: claim.PreviousModelInputGeneration,
			ReleasesInput:                claim.ReleasesInput,
			SessionAttributes:            map[string]any{"manager_id": traceOwner(trace)},
		}
		if len(claim.PreviousMessageIDs) > 0 {
			data.PreviousMessageAttributes = map[string]any{
				"message_ids": toAnySlice(materialize(claim.PreviousMessageIDs)),
			}
		}

		result := transport.Deliver(context.Background(), &managerdelivery.Item{
			ID: 1, AttemptID: "replay", Attempts: 1, Payload: data,
		})
		require.Empty(t, result.Error, "recorded production claims must deliver cleanly")

		lastDelivered = result.MessageIDs
	}

	return calls
}

func fmtRS(id int64) string {
	return strconv.FormatInt(id, 10)
}

func toAnySlice(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}

	return out
}
