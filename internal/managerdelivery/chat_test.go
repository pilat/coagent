package managerdelivery

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/logger"
)

type chatFake struct {
	focus      int64
	rows       map[int64][]ChatRow
	seen       map[int64]int64
	killed     map[int64]bool
	sessions   []ChatSession
	reads      []int64
	sent       []string
	results    []SendResult
	markErr    error
	onSend     func(string)
	onBoundary func(string)
}

type chatSendFunc func(context.Context, string) SendResult

func TestChatLoop_StartupTailAndFocusChange(t *testing.T) {
	fake := newChatFake()
	fake.focus = 12
	fake.rows[12] = []ChatRow{
		{ID: 1, Type: "session_opened"},
		{ID: 2, Type: "message", Content: "first"},
		{ID: 3, Type: "session_replaced"},
		{ID: 4, Type: "message", Content: "second"},
	}
	fake.rows[9] = []ChatRow{{ID: 10, Type: "message", Content: "other tail"}}
	fake.sessions = []ChatSession{
		{ID: 0, Name: "internal assistant", Unread: 1, LatestUnread: "assistant\nsecond line", LatestUnreadID: 25},
		{ID: 9, Name: "project - 9", Unread: 2, LatestUnread: strings.Repeat("界", 121), LatestUnreadID: 20},
		{ID: 12, Name: "project - 12", Unread: 1, LatestUnread: "excluded", LatestUnreadID: 50},
		{ID: 15, Name: "read", LatestUnreadID: 60},
	}
	w := NewChat(fake, fake).(*chatLoop)
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, []string{"first", "second"}, fake.sent)
	assert.Equal(t, int64(4), fake.seen[12])
	assert.Equal(t, int64(12), w.Focus())
	assert.NotContains(t, fake.reads, int64(9))

	fake.focus = 9
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, []string{
		"first", "second", "→ #9 project - 9",
		"#12 project - 12 · 1 new · excluded\n#0 Assistant · 1 new · assistant", "other tail",
	}, fake.sent)
	assert.Zero(t, fake.seen[0])

	fake.focus = 12
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, "→ #12 project - 12", fake.sent[5])
	assert.Equal(t,
		"#0 Assistant · 1 new · assistant\n#9 project - 9 · 2 new · "+strings.Repeat("界", 120),
		fake.sent[6],
	)
}

func TestChatLoop_SwitchToAssistantWithoutUnreadDigest(t *testing.T) {
	fake := newChatFake()
	fake.focus = 12
	w := NewChat(fake, fake).(*chatLoop)
	require.Zero(t, w.pass(t.Context()))
	fake.focus = 0
	fake.rows[0] = []ChatRow{{ID: 5, Type: "message", Content: "assistant tail"}}
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, []string{"→ #0 Assistant", "assistant tail"}, fake.sent)
	assert.Equal(t, []int64{12, 0, 0}, fake.reads)
}

func TestChatLoop_SwitchDuringLongTail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := newChatFake()
		fake.focus = 12
		for id := int64(1); id <= 125; id++ {
			fake.rows[12] = append(fake.rows[12], ChatRow{ID: id, Type: "message", Content: fmt.Sprintf("A%d", id)})
		}

		fake.rows[9] = []ChatRow{{ID: 126, Type: "message", Content: "B answer"}}
		fake.sessions = []ChatSession{
			{ID: 12, Name: "A", Unread: 124, LatestUnread: "A125", LatestUnreadID: 125},
			{ID: 9, Name: "B", Unread: 1, LatestUnread: "B answer", LatestUnreadID: 126},
		}
		fake.onSend = func(text string) {
			if text == "A1" {
				fake.focus = 9
			}
		}
		fake.onBoundary = func(point string) {
			if point == "before killed" {
				t.Fatal("switched pass must not inspect the old session's kill state")
			}
		}
		w := NewChat(fake, fake).(*chatLoop)

		require.Zero(t, w.pass(t.Context()))
		assert.Equal(t, []string{"A1"}, fake.sent)
		assert.Equal(t, int64(1), fake.seen[12])
		assert.Equal(t, []int64{12}, fake.reads)
		fake.onBoundary = nil
		before := time.Now()
		require.True(t, w.wait(t.Context(), 0))
		assert.Equal(t, before, time.Now(), "switch must wake the next pass without a fallback delay")
		require.Zero(t, w.pass(t.Context()))
		assert.Equal(t, []string{"A1", "→ #9 B", "#12 A · 124 new · A125", "B answer"}, fake.sent)
		assert.Equal(t, int64(1), fake.seen[12])
		assert.Equal(t, int64(126), fake.seen[9])
		assert.Equal(t, []int64{12, 9, 9}, fake.reads)
	})
}

func TestChatLoop_SwitchDuringKilledTailSkipsReset(t *testing.T) {
	fake := newChatFake()
	fake.focus = 12
	fake.killed[12] = true
	fake.onBoundary = func(point string) {
		switch point {
		case "after killed":
			fake.rows[12] = []ChatRow{
				{ID: 1, Type: "session_closed", Content: "Session killed."},
				{ID: 2, Type: "message", Content: "leave unread"},
			}
		case "before reset":
			t.Fatal("switched killed tail must not attempt a reset")
		}
	}
	fake.onSend = func(string) { fake.focus = 9 }
	w := NewChat(fake, fake).(*chatLoop)

	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, []string{"Session killed."}, fake.sent)
	assert.Equal(t, int64(1), fake.seen[12])
	assert.Equal(t, int64(9), fake.focus)
}

func TestChatLoop_TransientRetriesUntilSuccess(t *testing.T) {
	fake := newChatFake()
	fake.rows[0] = []ChatRow{{ID: 1, Type: "message", Content: "answer"}}
	fake.results = []SendResult{
		{Class: SendTransient}, {Class: SendTransient}, {Class: SendTransient}, {},
	}
	w := NewChat(fake, fake).(*chatLoop)
	for _, delay := range []time.Duration{3 * time.Second, 6 * time.Second, 12 * time.Second} {
		require.Equal(t, delay, w.pass(t.Context()))
		assert.Zero(t, fake.seen[0])
	}

	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, int64(1), fake.seen[0])
	assert.Equal(t, []string{"answer", "answer", "answer", "answer"}, fake.sent)
}

func TestChatLoop_PermanentRowProbesBeforeMarking(t *testing.T) {
	for _, test := range []struct {
		name  string
		probe SendClass
		seen  int64
		delay time.Duration
	}{
		{name: "delivered", probe: SendOK, seen: 1},
		{name: "transient", probe: SendTransient, delay: 3 * time.Second},
		{name: "permanent", probe: SendRowPermanent, delay: 3 * time.Second},
		{name: "dead chat", probe: SendChatFailure, delay: 3 * time.Second},
	} {
		t.Run(test.name, func(t *testing.T) {
			fake := newChatFake()
			fake.rows[0] = []ChatRow{{ID: 1, Type: "message", Content: "bad row"}}
			fake.results = []SendResult{{Class: SendRowPermanent, Reason: "too long"}, {Class: test.probe}}
			fake.onSend = func(string) { assert.Zero(t, fake.seen[0], "probe must precede the watermark") }
			core, logs := observer.New(zap.WarnLevel)
			ctx := logger.ToContext(t.Context(), zap.New(core))
			w := NewChat(fake, fake).(*chatLoop)
			assert.Equal(t, test.delay, w.pass(ctx))
			assert.Equal(t, []string{"bad row", "⚠ output #1 not delivered: too long"}, fake.sent)
			assert.Equal(t, test.seen, fake.seen[0])
			if test.probe == SendRowPermanent || test.probe == SendChatFailure {
				assert.Equal(t, 1, logs.FilterMessage("chat delivery paused").Len())
			}

			if test.probe == SendTransient {
				fake.onSend = nil
				fake.results = []SendResult{{Class: SendRowPermanent, Reason: "too long"}, {}}
				require.Zero(t, w.pass(ctx))
				assert.Equal(t, int64(1), fake.seen[0])
				assert.Len(t, fake.sent, 4)
			}
		})
	}
}

func TestChatLoop_ChatPauseBackoffAndRetryAfter(t *testing.T) {
	fake := newChatFake()
	fake.rows[0] = []ChatRow{{ID: 1, Type: "message", Content: "answer"}}
	w := NewChat(fake, fake).(*chatLoop)
	for _, delay := range []int{3, 6, 12, 24, 48, 60, 60, 60} {
		fake.results = []SendResult{{Class: SendChatFailure, Reason: "blocked"}}
		assert.Equal(t, time.Duration(delay)*time.Second, w.pass(t.Context()))
		assert.Zero(t, fake.seen[0])
	}

	fake.results = []SendResult{{Class: SendTransient, RetryAfter: 90 * time.Second}}
	assert.Equal(t, 90*time.Second, w.pass(t.Context()))
	assert.Zero(t, fake.seen[0])
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, int64(1), fake.seen[0])
}

func TestChatLoop_RestartResendsOnlyUnmarkedRow(t *testing.T) {
	fake := newChatFake()
	fake.rows[0] = []ChatRow{
		{ID: 1, Type: "message", Content: "before"},
		{ID: 2, Type: "message", Content: "crash here"},
		{ID: 3, Type: "message", Content: "after"},
	}
	ctx, cancel := context.WithCancel(t.Context())
	fake.onSend = func(text string) {
		if text == "crash here" {
			cancel()
		}
	}
	w := NewChat(fake, fake).(*chatLoop)
	w.pass(ctx)
	assert.Equal(t, int64(1), fake.seen[0])
	fake.onSend = nil
	w = NewChat(fake, fake).(*chatLoop)
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, []string{"before", "crash here", "crash here", "after"}, fake.sent)
	assert.Equal(t, int64(3), fake.seen[0])
}

func TestChatLoop_BatchesAndMarkFailure(t *testing.T) {
	fake := newChatFake()
	for id := int64(1); id <= 125; id++ {
		fake.rows[0] = append(fake.rows[0], ChatRow{ID: id, Type: "message", Content: fmt.Sprint(id)})
	}

	w := NewChat(fake, fake).(*chatLoop)
	fake.markErr = errors.New("unavailable")
	assert.Equal(t, 3*time.Second, w.pass(t.Context()))
	assert.Zero(t, fake.seen[0])
	fake.markErr = nil
	require.Zero(t, w.pass(t.Context()))
	assert.Len(t, fake.sent, 126)
	assert.Equal(t, []int64{0, 0, 0, 0}, fake.reads)
	assert.Equal(t, int64(125), fake.seen[0])
}

func TestChatLoop_WakeFallbackAndRetryDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := newChatFake()
		fake.rows[0] = []ChatRow{{ID: 1, Type: "message", Content: "retry"}}
		fake.results = []SendResult{{Class: SendTransient, RetryAfter: 10 * time.Second}}
		w := NewChat(fake, fake)
		w.Start(t.Context())
		synctest.Wait()
		require.Len(t, fake.sent, 1)
		for range 1000 {
			w.Wake()
		}

		time.Sleep(10*time.Second - time.Nanosecond)
		synctest.Wait()
		assert.Len(t, fake.sent, 1)
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		assert.Equal(t, int64(1), fake.seen[0])
		fake.rows[0] = append(fake.rows[0], ChatRow{ID: 2, Type: "message", Content: "fallback"})
		time.Sleep(5 * time.Second)
		synctest.Wait()
		assert.Equal(t, int64(2), fake.seen[0])
		fake.rows[0] = append(fake.rows[0], ChatRow{ID: 3, Type: "message", Content: "woken"})
		w.Wake()
		synctest.Wait()
		assert.Equal(t, int64(3), fake.seen[0])
		require.NoError(t, w.Stop(t.Context()))
	})
}

func TestChatLoop_SwitchPreludeResumesWithoutRepeatingHeader(t *testing.T) {
	fake := newChatFake()
	w := NewChat(fake, fake).(*chatLoop)
	require.Zero(t, w.pass(t.Context()))
	fake.focus = 4
	fake.sessions = []ChatSession{
		{ID: 4, Name: "work"}, {ID: 0, Name: "Assistant", Unread: 1, LatestUnread: "new"},
	}
	fake.rows[4] = []ChatRow{{ID: 1, Type: "message", Content: "tail"}}
	fake.results = []SendResult{{}, {Class: SendTransient}}
	assert.Equal(t, 3*time.Second, w.pass(t.Context()))
	assert.Empty(t, fake.reads[1:])
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t,
		[]string{"→ #4 work", "#0 Assistant · 1 new · new", "#0 Assistant · 1 new · new", "tail"},
		fake.sent,
	)
}

func TestChatLoop_ProbeSkipContinuesToNextRow(t *testing.T) {
	fake := newChatFake()
	fake.rows[0] = []ChatRow{
		{ID: 1, Type: "message", Content: "bad"},
		{ID: 2, Type: "message", Content: "next"},
	}
	fake.results = []SendResult{{Class: SendRowPermanent, Reason: "invalid"}, {}, {}}
	w := NewChat(fake, fake).(*chatLoop)
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, []string{"bad", "⚠ output #1 not delivered: invalid", "next"}, fake.sent)
	assert.Equal(t, int64(2), fake.seen[0])
}

func TestChatLoop_StopCancelsSendWithoutMarking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := newChatFake()
		fake.rows[0] = []ChatRow{{ID: 1, Type: "message", Content: "answer"}}
		w := NewChat(fake, chatSendFunc(func(ctx context.Context, _ string) SendResult {
			<-ctx.Done()
			return SendResult{Class: SendTransient, Reason: ctx.Err().Error()}
		}))
		w.Start(t.Context())
		w.Start(t.Context())
		synctest.Wait()
		require.NoError(t, w.Stop(t.Context()))
		assert.Zero(t, fake.seen[0])
		assert.Len(t, fake.reads, 1)
		require.NoError(t, w.Stop(t.Context()))
	})
}

func TestChatLoop_StopBeforeStartPreventsWork(t *testing.T) {
	fake := newChatFake()
	w := NewChat(fake, fake)
	require.NoError(t, w.Stop(t.Context()))
	w.Start(t.Context())
	assert.Empty(t, fake.reads)
}

func TestChatLoop_PortPanicIsRecoveredAndLogged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := newChatFake()
		fake.rows[0] = []ChatRow{{ID: 1, Type: "message", Content: "unmarked answer"}}
		fake.onBoundary = func(boundary string) {
			if boundary == "before mark" {
				panic("mark port failed")
			}
		}
		core, logs := observer.New(zap.ErrorLevel)
		ctx := logger.ToContext(t.Context(), zap.New(core))
		w := NewChat(fake, fake).(*chatLoop)
		w.Start(ctx)
		synctest.Wait()
		select {
		case <-w.done:
		default:
			t.Fatal("panicking loop did not finish its lifecycle")
		}

		assert.Equal(t, []string{"unmarked answer"}, fake.sent)
		assert.Zero(t, fake.seen[0])
		entries := logs.FilterMessage("chat loop panicked").All()
		require.Len(t, entries, 1)
		assert.Equal(t, "mark port failed", entries[0].ContextMap()["recovered"])
		assert.NotEmpty(t, entries[0].ContextMap()["stack"])
		require.NoError(t, w.Stop(t.Context()))
	})
}

func (fn chatSendFunc) Send(ctx context.Context, text string) SendResult { return fn(ctx, text) }

func newChatFake() *chatFake {
	return &chatFake{
		rows: make(map[int64][]ChatRow), seen: make(map[int64]int64), killed: make(map[int64]bool),
	}
}

func (f *chatFake) boundary(name string) {
	if f.onBoundary != nil {
		f.onBoundary(name)
	}
}

func (f *chatFake) Focus(context.Context) (int64, error) {
	f.boundary("before focus")
	focus := f.focus
	f.boundary("after focus")

	return focus, nil
}

func (f *chatFake) ResetFocus(_ context.Context, from int64) (bool, error) {
	f.boundary("before reset")
	reset := f.focus == from
	if reset {
		f.focus = 0
	}

	f.boundary("after reset")

	return reset, nil
}

func (f *chatFake) Outputs(_ context.Context, sessionID int64, limit int) ([]ChatRow, error) {
	f.boundary("before outputs")
	f.reads = append(f.reads, sessionID)

	var rows []ChatRow

	for _, row := range f.rows[sessionID] {
		if row.ID > f.seen[sessionID] {
			rows = append(rows, row)
			if len(rows) == limit {
				break
			}
		}
	}

	f.boundary("after outputs")

	return rows, nil
}

func (f *chatFake) MarkSeen(_ context.Context, sessionID, id int64) error {
	f.boundary("before mark")
	if f.markErr != nil {
		return f.markErr
	}

	f.seen[sessionID] = max(f.seen[sessionID], id)
	f.boundary("after mark")

	return nil
}

func (f *chatFake) Sessions(context.Context) ([]ChatSession, error) {
	f.boundary("before sessions")
	rows := append([]ChatSession(nil), f.sessions...)
	f.boundary("after sessions")

	return rows, nil
}

func (f *chatFake) Killed(_ context.Context, sessionID int64) (bool, error) {
	f.boundary("before killed")
	killed := f.killed[sessionID]
	f.boundary("after killed")

	return killed, nil
}

func (f *chatFake) Send(_ context.Context, text string) SendResult {
	f.boundary("before send")
	f.sent = append(f.sent, text)
	if f.onSend != nil {
		f.onSend(text)
	}

	var result SendResult

	if len(f.results) > 0 {
		result, f.results = f.results[0], f.results[1:]
	}

	f.boundary("after send")

	return result
}
