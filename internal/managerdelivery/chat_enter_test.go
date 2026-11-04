package managerdelivery

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatEnterSameFocusSendsPrelude(t *testing.T) {
	t.Parallel()
	fake := newChatFake()
	fake.rows[7] = []ChatRow{{ID: 10, Type: "message", Content: "work context"}}
	fake.cursor = 10
	fake.sessions = []ChatSession{
		{ID: 7, Name: "selected"},
		{ID: 12, Name: "work"},
	}
	w := NewChat(fake, fake).(*chatLoop)
	require.Zero(t, w.pass(t.Context()))
	require.Empty(t, fake.sent)
	fake.selectFocus(7)
	require.Zero(t, w.pass(t.Context()))
	want := []string{"→ #7 selected", "#7 selected\nwork context"}
	assert.Equal(t, want, fake.sent)
	w.Wake()
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, want, fake.sent)
	assert.EqualValues(t, 10, fake.cursor)
}

func TestChatEnterBeforeFirstPassSendsPrelude(t *testing.T) {
	t.Parallel()
	fake := newChatFake()
	fake.sessions = []ChatSession{{ID: 7, Name: "selected"}}
	w := NewChat(fake, fake).(*chatLoop)
	fake.selectFocus(7)
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, []string{"→ #7 selected"}, fake.sent)
	w.Wake()
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, []string{"→ #7 selected"}, fake.sent)
}

func TestChatEnterWaitsForCommittedFocus(t *testing.T) {
	for _, initialized := range []bool{false, true} {
		t.Run(fmt.Sprint(initialized), func(t *testing.T) {
			t.Parallel()
			fake := newChatFake()
			fake.sessions = []ChatSession{{ID: 12, Name: "work"}}
			w := NewChat(fake, fake).(*chatLoop)
			if initialized {
				require.Zero(t, w.pass(t.Context()))
			}
			require.Zero(t, w.pass(t.Context()))
			assert.Empty(t, fake.sent)
			fake.selectFocus(12)
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
	fake.selectFocus(12)
	fake.onBoundary = func(point string) {
		if point == "after sessions" {
			fake.selectFocus(9)
		}
	}
	require.Zero(t, w.pass(t.Context()))
	fake.onBoundary = nil
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, []string{"→ #12 first", "→ #9 second"}, fake.sent)
	w.Wake()
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, []string{"→ #12 first", "→ #9 second"}, fake.sent)
}

func TestChatEntry_RowPermanentFailureProbesBeforeEntryCAS(t *testing.T) {
	for _, probe := range []SendClass{SendOK, SendTransient, SendRowPermanent, SendChatFailure} {
		t.Run(fmt.Sprint(probe), func(t *testing.T) {
			fake := newChatFake()
			fake.selectFocus(7)
			fake.sessions = []ChatSession{{ID: 7, Name: "work"}}
			fake.rows[7] = []ChatRow{{ID: 4, Type: "message", Content: "bad row"}}
			fake.results = []SendResult{{}, {Class: SendRowPermanent, Reason: "invalid"}, {Class: probe}}
			fake.onSend = func(string) { assert.False(t, fake.entered) }
			loop := NewChat(fake, fake).(*chatLoop)
			delay := loop.pass(t.Context())
			assert.Equal(t, []string{"→ #7 work", "#7 work\nbad row", "⚠ output #4 not delivered: invalid"}, fake.sent)
			if probe == SendOK {
				assert.Zero(t, delay)
				assert.True(t, fake.entered)
				assert.EqualValues(t, 4, fake.cursor)
			} else {
				assert.Equal(t, 3*time.Second, delay)
				assert.False(t, fake.entered)
				assert.Zero(t, fake.cursor)
			}
		})
	}
}
