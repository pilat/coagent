package managerdelivery

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/pilat/coagent/internal/logger"
)

func TestChatLoop_EntryTailOrSeenContext(t *testing.T) {
	for _, unread := range []bool{false, true} {
		t.Run(fmt.Sprint(unread), func(t *testing.T) {
			t.Parallel()
			fake := newChatFake()
			fake.rows[12] = []ChatRow{{ID: 10, Type: "message", Content: "answer"}}
			fake.sessions = []ChatSession{{ID: 12, Name: "work"}, {ID: 9, Unread: 1, LatestUnread: "other"}}
			if unread {
				fake.sessions[0].Unread = 1
			} else {
				fake.seen[12] = 10
			}
			fake.onBoundary = func(point string) {
				if point == "before last seen" && unread {
					t.Fatal("unread entry must not request context")
				}
			}
			w := NewChat(fake, fake).(*chatLoop)
			require.Zero(t, w.pass(t.Context()))
			fake.focus = 12
			require.Zero(t, w.pass(t.Context()))
			assert.Equal(t, []string{"→ #12 work", "#12 work\nanswer"}, fake.sent)
			assert.EqualValues(t, 10, fake.seen[12])
			assert.Zero(t, fake.seen[9])
		})
	}
}

func TestChatLoop_AssistantEntryDigestThenContext(t *testing.T) {
	t.Parallel()
	fake := newChatFake()
	fake.focus = 12
	fake.rows[0] = []ChatRow{{ID: 10, Type: "message", Content: "assistant answer"}}
	fake.seen[0] = 10
	fake.sessions = []ChatSession{
		{ID: 0, Name: "Assistant"},
		{ID: 12, Name: "work", Unread: 1, LatestUnread: "other answer", LatestUnreadID: 20},
	}
	w := NewChat(fake, fake).(*chatLoop)
	require.Zero(t, w.pass(t.Context()))
	fake.focus = 0
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, []string{
		"→ #0 Assistant", "#12 work · 1 new · other answer", "assistant answer",
	}, fake.sent)
	assert.EqualValues(t, 10, fake.seen[0])
	assert.Zero(t, fake.seen[12])
}

func TestChatLoop_EntryContextSendFailures(t *testing.T) {
	for _, class := range []SendClass{SendRowPermanent, SendTransient, SendChatFailure} {
		t.Run(fmt.Sprint(class), func(t *testing.T) {
			t.Parallel()
			fake := newChatFake()
			fake.rows[12] = []ChatRow{{ID: 10, Type: "message", Content: "context"}}
			fake.seen[12] = 10
			fake.sessions = []ChatSession{{ID: 12, Name: "work"}}
			w := NewChat(fake, fake).(*chatLoop)
			require.Zero(t, w.pass(t.Context()))
			fake.focus = 12
			fake.results = []SendResult{{}, {Class: class, RetryAfter: 8 * time.Second}}
			core, logs := observer.New(zap.WarnLevel)
			ctx := logger.ToContext(t.Context(), zap.New(core))
			delay := w.pass(ctx)
			assert.EqualValues(t, 10, fake.seen[12])
			if class == SendRowPermanent {
				assert.Zero(t, delay)
				assert.Empty(t, w.prelude)
				assert.Zero(t, logs.FilterMessage("chat delivery paused").Len())
			} else {
				assert.Equal(t, 8*time.Second, delay)
				require.Len(t, w.prelude, 1)
				require.Zero(t, w.pass(ctx))
				assert.Equal(t, []string{"→ #12 work", "#12 work\ncontext", "#12 work\ncontext"}, fake.sent)
			}
			fake.rows[12] = append(fake.rows[12], ChatRow{ID: 11, Type: "message", Content: "next"})
			require.Zero(t, w.pass(ctx))
			assert.Equal(t, "#12 work\nnext", fake.sent[len(fake.sent)-1])
			assert.EqualValues(t, 11, fake.seen[12])
		})
	}
}

func TestChatLoop_EntryContextDoesNotDuplicateNewTail(t *testing.T) {
	t.Parallel()
	fake := newChatFake()
	fake.rows[12] = []ChatRow{{ID: 10, Type: "message", Content: "seen"}}
	fake.seen[12] = 10
	fake.sessions = []ChatSession{{ID: 12, Name: "work"}}
	w := NewChat(fake, fake).(*chatLoop)
	require.Zero(t, w.pass(t.Context()))
	fake.focus = 12
	fake.onBoundary = func(point string) {
		if point == "after sessions" {
			fake.rows[12] = append(fake.rows[12], ChatRow{ID: 11, Type: "message", Content: "new"})
		}
	}
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, []string{"→ #12 work", "#12 work\nseen", "#12 work\nnew"}, fake.sent)
	assert.EqualValues(t, 11, fake.seen[12])
}

func TestChatLoop_RenameAppliesToNextOutputBatch(t *testing.T) {
	t.Parallel()
	fake := newChatFake()
	fake.focus = 12
	fake.sessions = []ChatSession{{ID: 12, Name: "old title"}}
	for id := int64(1); id <= chatBatchSize+1; id++ {
		fake.rows[12] = append(fake.rows[12], ChatRow{ID: id, Type: "message", Content: fmt.Sprint(id)})
	}
	fake.onSend = func(string) { fake.sessions[0].Name = "new title" }
	w := NewChat(fake, fake).(*chatLoop)
	require.Zero(t, w.pass(t.Context()))
	require.Len(t, fake.sent, chatBatchSize+1)
	for i, text := range fake.sent[:chatBatchSize] {
		assert.Equal(t, fmt.Sprintf("#12 old title\n%d", i+1), text)
	}
	assert.Equal(t, "#12 new title\n101", fake.sent[chatBatchSize])
}
