package managerdelivery

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatEnterSameFocusSendsPrelude(t *testing.T) {
	t.Parallel()
	fake := newChatFake()
	fake.rows[0] = []ChatRow{{ID: 10, Type: "message", Content: "assistant context"}}
	fake.seen[0] = 10
	fake.sessions = []ChatSession{
		{ID: 0, Name: "Assistant"},
		{ID: 12, Name: "work", Unread: 1, LatestUnread: "work answer", LatestUnreadID: 20},
	}
	w := NewChat(fake, fake).(*chatLoop)
	require.Zero(t, w.pass(t.Context()))
	require.Empty(t, fake.sent)
	w.Enter(0)
	require.Zero(t, w.pass(t.Context()))
	want := []string{"→ #0 Assistant", "#12 work · 1 new · work answer", "assistant context"}
	assert.Equal(t, want, fake.sent)
	w.Wake()
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, want, fake.sent)
	assert.EqualValues(t, 10, fake.seen[0])
	assert.Zero(t, fake.seen[12])
}

func TestChatEnterBeforeFirstPassSendsPrelude(t *testing.T) {
	t.Parallel()
	fake := newChatFake()
	w := NewChat(fake, fake).(*chatLoop)
	w.Enter(0)
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, []string{"→ #0 Assistant"}, fake.sent)
	w.Wake()
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, []string{"→ #0 Assistant"}, fake.sent)
}

func TestChatEnterWaitsForTargetFocus(t *testing.T) {
	for _, initialized := range []bool{false, true} {
		t.Run(fmt.Sprint(initialized), func(t *testing.T) {
			t.Parallel()
			fake := newChatFake()
			fake.sessions = []ChatSession{{ID: 12, Name: "work"}}
			w := NewChat(fake, fake).(*chatLoop)
			if initialized {
				require.Zero(t, w.pass(t.Context()))
			}
			w.Enter(12)
			require.Zero(t, w.pass(t.Context()))
			assert.Empty(t, fake.sent)
			fake.focus = 12
			require.Zero(t, w.pass(t.Context()))
			w.Wake()
			require.Zero(t, w.pass(t.Context()))
			assert.Equal(t, []string{"→ #12 work"}, fake.sent)
		})
	}
}

func TestChatEnterPreservesDifferentTargetDuringPreparation(t *testing.T) {
	t.Parallel()
	fake := newChatFake()
	fake.sessions = []ChatSession{{ID: 12, Name: "first"}, {ID: 9, Name: "second"}}
	w := NewChat(fake, fake).(*chatLoop)
	require.Zero(t, w.pass(t.Context()))
	w.Enter(12)
	fake.focus = 12
	fake.onBoundary = func(point string) {
		if point == "after sessions" {
			w.Enter(9)
		}
	}
	require.Zero(t, w.pass(t.Context()))
	fake.onBoundary = nil
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, []string{"→ #12 first"}, fake.sent)
	fake.focus = 9
	require.Zero(t, w.pass(t.Context()))
	w.Wake()
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, []string{"→ #12 first", "→ #9 second"}, fake.sent)
}
