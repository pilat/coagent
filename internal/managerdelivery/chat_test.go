package managerdelivery

import (
	"context"
	"errors"
	"fmt"
	"sync"
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
	mu         sync.Mutex
	focus      int64
	rows       map[int64][]ChatRow
	cursor     int64
	entered    bool
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

func TestChatLoop_NoFocusDoesNotCallSessionPorts(t *testing.T) {
	fake := newChatFake()
	fake.focus = 0
	fake.onBoundary = func(point string) {
		require.Contains(t, []string{"before focus", "after focus"}, point)
	}
	w := NewChat(fake, fake).(*chatLoop)
	require.Zero(t, w.pass(t.Context()))
	assert.Empty(t, fake.sent)
	assert.Empty(t, fake.reads)
}

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
		{ID: 9, Name: "project - 9"},
		{ID: 12, Name: "project - 12"},
		{ID: 15, Name: "read"},
	}
	w := NewChat(fake, fake).(*chatLoop)
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, []string{"**#12 project - 12**\n\nfirst", "**#12 project - 12**\n\nsecond"}, fake.sent)
	assert.Equal(t, int64(4), fake.cursor)
	assert.Equal(t, int64(12), w.Focus())
	assert.NotContains(t, fake.reads, int64(9))

	fake.selectFocus(9)
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, []string{
		"**#12 project - 12**\n\nfirst", "**#12 project - 12**\n\nsecond", "**→ #9 project - 9**",
		"**#9 project - 9**\n\nother tail",
	}, fake.sent)
	assert.EqualValues(t, 10, fake.cursor)

	fake.selectFocus(12)
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, "**→ #12 project - 12**", fake.sent[4])
	assert.Len(t, fake.sent, 7)
	fake.focus = 0
	require.Zero(t, w.pass(t.Context()))
	assert.Len(t, fake.sent, 7)
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
			{ID: 12, Name: "A"},
			{ID: 9, Name: "B"},
		}
		fake.onSend = func(text string) {
			if text == "**#12 A**\n\nA1" {
				fake.selectFocus(9)
			}
		}
		fake.onBoundary = func(point string) {
			if point == "before killed" {
				t.Fatal("switched pass must not inspect the old session's kill state")
			}
		}
		w := NewChat(fake, fake).(*chatLoop)

		require.Zero(t, w.pass(t.Context()))
		assert.Equal(t, []string{"**#12 A**\n\nA1"}, fake.sent)
		assert.Zero(t, fake.cursor)
		assert.False(t, fake.entered)
		assert.Equal(t, []int64{12}, fake.reads)
		fake.onBoundary = nil
		before := time.Now()
		require.True(t, w.wait(t.Context(), 0))
		assert.Equal(t, before, time.Now(), "switch must wake the next pass without a fallback delay")
		require.Zero(t, w.pass(t.Context()))
		assert.Equal(t, []string{"**#12 A**\n\nA1", "**→ #9 B**", "**#9 B**\n\nB answer"}, fake.sent)
		assert.Equal(t, int64(126), fake.cursor)
		assert.Equal(t, []int64{12, 9}, fake.reads)
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
				{ID: 2, Type: "message", Content: "later old row"},
			}
		case "before reset":
			t.Fatal("switched killed tail must not attempt a reset")
		}
	}
	fake.onSend = func(string) { fake.selectFocus(9) }
	w := NewChat(fake, fake).(*chatLoop)

	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, []string{"**#12 project - 12**\n\nSession killed."}, fake.sent)
	assert.Zero(t, fake.cursor)
	assert.False(t, fake.entered)
	assert.Equal(t, int64(9), fake.focus)
}

func TestChatLoop_TransientRetriesUntilSuccess(t *testing.T) {
	fake := newChatFake()
	fake.rows[7] = []ChatRow{{ID: 1, Type: "message", Content: "answer"}}
	fake.results = []SendResult{
		{Class: SendTransient}, {Class: SendTransient}, {Class: SendTransient}, {},
	}
	w := NewChat(fake, fake).(*chatLoop)
	for _, delay := range []time.Duration{3 * time.Second, 6 * time.Second, 12 * time.Second} {
		require.Equal(t, delay, w.pass(t.Context()))
		assert.Zero(t, fake.cursor)
	}

	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, int64(1), fake.cursor)
	assert.Equal(
		t,
		[]string{
			"**#7 project - 7**\n\nanswer",
			"**#7 project - 7**\n\nanswer",
			"**#7 project - 7**\n\nanswer",
			"**#7 project - 7**\n\nanswer",
		},
		fake.sent,
	)
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
			fake.rows[7] = []ChatRow{{ID: 1, Type: "message", Content: "bad row"}}
			fake.results = []SendResult{{Class: SendRowPermanent, Reason: "too long"}, {Class: test.probe}}
			fake.onSend = func(string) { assert.Zero(t, fake.cursor, "probe must precede the cursor advance") }
			core, logs := observer.New(zap.WarnLevel)
			ctx := logger.ToContext(t.Context(), zap.New(core))
			w := NewChat(fake, fake).(*chatLoop)
			assert.Equal(t, test.delay, w.pass(ctx))
			assert.Equal(t, []string{"**#7 project - 7**\n\nbad row", "⚠ output #1 not delivered: too long"}, fake.sent)
			assert.Equal(t, test.seen, fake.cursor)
			if test.probe == SendRowPermanent || test.probe == SendChatFailure {
				assert.Equal(t, 1, logs.FilterMessage("chat delivery paused").Len())
			}

			if test.probe == SendTransient {
				fake.onSend = nil
				fake.results = []SendResult{{Class: SendRowPermanent, Reason: "too long"}, {}}
				require.Zero(t, w.pass(ctx))
				assert.Equal(t, int64(1), fake.cursor)
				assert.Len(t, fake.sent, 4)
			}
		})
	}
}

func TestChatLoop_ChatPauseBackoffAndRetryAfter(t *testing.T) {
	fake := newChatFake()
	fake.rows[7] = []ChatRow{{ID: 1, Type: "message", Content: "answer"}}
	w := NewChat(fake, fake).(*chatLoop)
	for _, delay := range []int{3, 6, 12, 24, 48, 60, 60, 60} {
		fake.results = []SendResult{{Class: SendChatFailure, Reason: "blocked"}}
		assert.Equal(t, time.Duration(delay)*time.Second, w.pass(t.Context()))
		assert.Zero(t, fake.cursor)
	}

	fake.results = []SendResult{{Class: SendTransient, RetryAfter: 90 * time.Second}}
	assert.Equal(t, 90*time.Second, w.pass(t.Context()))
	assert.Zero(t, fake.cursor)
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, int64(1), fake.cursor)
}

func TestChatLoop_RestartResendsOnlyUnmarkedRow(t *testing.T) {
	fake := newChatFake()
	fake.rows[7] = []ChatRow{
		{ID: 1, Type: "message", Content: "before"},
		{ID: 2, Type: "message", Content: "crash here"},
		{ID: 3, Type: "message", Content: "after"},
	}
	ctx, cancel := context.WithCancel(t.Context())
	fake.onSend = func(text string) {
		if text == "**#7 project - 7**\n\ncrash here" {
			cancel()
		}
	}
	w := NewChat(fake, fake).(*chatLoop)
	w.pass(ctx)
	assert.Equal(t, int64(1), fake.cursor)
	fake.onSend = nil
	w = NewChat(fake, fake).(*chatLoop)
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(
		t,
		[]string{
			"**#7 project - 7**\n\nbefore",
			"**#7 project - 7**\n\ncrash here",
			"**#7 project - 7**\n\ncrash here",
			"**#7 project - 7**\n\nafter",
		},
		fake.sent,
	)
	assert.Equal(t, int64(3), fake.cursor)
}

func TestChatLoop_BatchesAndMarkFailure(t *testing.T) {
	fake := newChatFake()
	for id := int64(1); id <= 125; id++ {
		fake.rows[7] = append(fake.rows[7], ChatRow{ID: id, Type: "message", Content: fmt.Sprint(id)})
	}

	w := NewChat(fake, fake).(*chatLoop)
	fake.markErr = errors.New("unavailable")
	assert.Equal(t, 3*time.Second, w.pass(t.Context()))
	assert.Zero(t, fake.cursor)
	fake.markErr = nil
	require.Zero(t, w.pass(t.Context()))
	assert.Len(t, fake.sent, 126)
	assert.Equal(t, []int64{7, 7, 7, 7}, fake.reads)
	assert.Equal(t, int64(125), fake.cursor)
}

func TestChatLoop_WakeFallbackAndRetryDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := newChatFake()
		fake.rows[7] = []ChatRow{{ID: 1, Type: "message", Content: "retry"}}
		fake.results = []SendResult{{Class: SendTransient, RetryAfter: 10 * time.Second}}
		w := NewChat(fake, fake)
		w.Start(t.Context())
		synctest.Wait()
		require.Len(t, fake.sentTexts(), 1)
		for range 1000 {
			w.Wake()
		}

		time.Sleep(10*time.Second - time.Nanosecond)
		synctest.Wait()
		assert.Len(t, fake.sentTexts(), 1)
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		assert.Equal(t, int64(1), fake.sentCursor())
		fake.appendRows(7, ChatRow{ID: 2, Type: "message", Content: "fallback"})
		time.Sleep(5 * time.Second)
		synctest.Wait()
		assert.Equal(t, int64(2), fake.sentCursor())
		fake.appendRows(7, ChatRow{ID: 3, Type: "message", Content: "woken"})
		w.Wake()
		synctest.Wait()
		assert.Equal(t, int64(3), fake.sentCursor())
		require.NoError(t, w.Stop(t.Context()))
	})
}

func TestChatLoop_SwitchPreludeResumesWithoutRepeatingHeader(t *testing.T) {
	fake := newChatFake()
	fake.focus = 4
	w := NewChat(fake, fake).(*chatLoop)
	require.Zero(t, w.pass(t.Context()))
	fake.selectFocus(7)
	fake.sessions = []ChatSession{{ID: 4, Name: "work"}, {ID: 7, Name: "selected"}}
	fake.rows[7] = []ChatRow{{ID: 1, Type: "message", Content: "context"}}
	fake.results = []SendResult{{}, {Class: SendTransient}}
	assert.Equal(t, 3*time.Second, w.pass(t.Context()))
	assert.Empty(t, fake.reads[1:])
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, []string{
		"**→ #7 selected**", "**#7 selected**\n\ncontext", "**#7 selected**\n\ncontext",
	}, fake.sent)
}

func TestChatLoop_ProbeSkipContinuesToNextRow(t *testing.T) {
	fake := newChatFake()
	fake.rows[7] = []ChatRow{
		{ID: 1, Type: "message", Content: "bad"},
		{ID: 2, Type: "message", Content: "next"},
	}
	fake.results = []SendResult{{Class: SendRowPermanent, Reason: "invalid"}, {}, {}}
	w := NewChat(fake, fake).(*chatLoop)
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(
		t,
		[]string{"**#7 project - 7**\n\nbad", "⚠ output #1 not delivered: invalid", "**#7 project - 7**\n\nnext"},
		fake.sent,
	)
	assert.Equal(t, int64(2), fake.cursor)
}

func TestChatLoop_StopCancelsSendWithoutMarking(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fake := newChatFake()
		fake.rows[7] = []ChatRow{{ID: 1, Type: "message", Content: "answer"}}
		w := NewChat(fake, chatSendFunc(func(ctx context.Context, _ string) SendResult {
			<-ctx.Done()
			return SendResult{Class: SendTransient, Reason: ctx.Err().Error()}
		}))
		w.Start(t.Context())
		w.Start(t.Context())
		synctest.Wait()
		require.NoError(t, w.Stop(t.Context()))
		assert.Zero(t, fake.cursor)
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
		fake.rows[7] = []ChatRow{{ID: 1, Type: "message", Content: "unmarked answer"}}
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

		assert.Equal(t, []string{"**#7 project - 7**\n\nunmarked answer"}, fake.sent)
		assert.Zero(t, fake.cursor)
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
		focus: 7, entered: true, rows: make(map[int64][]ChatRow), killed: make(map[int64]bool),
	}
}

func (f *chatFake) boundary(name string) {
	f.mu.Lock()
	onBoundary := f.onBoundary
	f.mu.Unlock()
	if onBoundary != nil {
		onBoundary(name)
	}
}

func (f *chatFake) Focus(context.Context) (FocusState, error) {
	f.boundary("before focus")
	f.mu.Lock()
	state := FocusState{Session: f.focus, Cursor: f.cursor, Entered: f.entered}
	f.mu.Unlock()
	f.boundary("after focus")
	return state, nil
}

func (f *chatFake) ResetFocus(_ context.Context, from int64) (bool, error) {
	f.boundary("before reset")
	f.mu.Lock()
	reset := f.focus == from
	if reset {
		f.focus, f.cursor, f.entered = 0, 0, false
	}
	f.mu.Unlock()

	f.boundary("after reset")

	return reset, nil
}

func (f *chatFake) Outputs(_ context.Context, sessionID, after int64, limit int) ([]ChatRow, error) {
	f.boundary("before outputs")
	f.mu.Lock()
	f.reads = append(f.reads, sessionID)

	var rows []ChatRow
	name := f.rowNameLocked(sessionID)

	for _, row := range f.rows[sessionID] {
		if row.ID > after {
			row.Name = name
			rows = append(rows, row)
			if len(rows) == limit {
				break
			}
		}
	}
	f.mu.Unlock()

	f.boundary("after outputs")

	return rows, nil
}

func (f *chatFake) Advance(_ context.Context, sessionID, id int64) (bool, error) {
	f.boundary("before mark")
	f.mu.Lock()
	if err := f.markErr; err != nil {
		f.mu.Unlock()
		return false, err
	}
	advanced := f.focus == sessionID && f.entered && f.cursor < id
	if advanced {
		f.cursor = id
	}
	f.mu.Unlock()
	f.boundary("after mark")
	return advanced, nil
}

func (f *chatFake) Entry(_ context.Context, sessionID int64, limit int) ([]ChatRow, int64, error) {
	f.boundary("before entry")
	f.mu.Lock()
	var rows []ChatRow
	var newest int64
	for _, row := range f.rows[sessionID] {
		newest = max(newest, row.ID)
		if row.Type != "session_opened" && row.Type != "session_replaced" {
			row.Name = f.rowNameLocked(sessionID)
			rows = append(rows, row)
		}
	}
	if len(rows) > limit {
		rows = rows[len(rows)-limit:]
	}
	f.mu.Unlock()
	f.boundary("after entry")
	return rows, newest, nil
}

func (f *chatFake) Enter(_ context.Context, sessionID, newest int64) (bool, error) {
	f.boundary("before enter")
	f.mu.Lock()
	entered := f.focus == sessionID && !f.entered
	if entered {
		f.entered, f.cursor = true, newest
	}
	f.mu.Unlock()
	f.boundary("after enter")
	return entered, nil
}

func (f *chatFake) selectFocus(sessionID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.focus, f.cursor, f.entered = sessionID, 0, false
}

func (f *chatFake) rowNameLocked(sessionID int64) string {
	for _, session := range f.sessions {
		if session.ID == sessionID {
			return session.Name
		}
	}
	return fmt.Sprintf("project - %d", sessionID)
}

func (f *chatFake) Sessions(context.Context) ([]ChatSession, error) {
	f.boundary("before sessions")
	f.mu.Lock()
	rows := append([]ChatSession(nil), f.sessions...)
	f.mu.Unlock()
	f.boundary("after sessions")

	return rows, nil
}

func (f *chatFake) Killed(_ context.Context, sessionID int64) (bool, error) {
	f.boundary("before killed")
	f.mu.Lock()
	killed := f.killed[sessionID]
	f.mu.Unlock()
	f.boundary("after killed")

	return killed, nil
}

func (f *chatFake) Send(_ context.Context, text string) SendResult {
	f.boundary("before send")
	f.mu.Lock()
	f.sent = append(f.sent, text)
	onSend := f.onSend
	f.mu.Unlock()
	if onSend != nil {
		onSend(text)
	}

	var result SendResult

	f.mu.Lock()
	if len(f.results) > 0 {
		result, f.results = f.results[0], f.results[1:]
	}
	f.mu.Unlock()

	f.boundary("after send")

	return result
}

func (f *chatFake) appendRows(sessionID int64, rows ...ChatRow) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[sessionID] = append(f.rows[sessionID], rows...)
}

func (f *chatFake) sentTexts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

func (f *chatFake) sentCursor() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cursor
}
