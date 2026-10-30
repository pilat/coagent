package managerdelivery

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	killPassBoundaryLimit = 40
	switchTailSize        = 125
)

// The oracle models obligations, not the loop's sequence of reads.
type killDeliveryModel struct {
	terminating bool
	killed      bool
	reset       bool
	delivered   []string
	prelude     []string
}

type switchDeliveryModel struct {
	cutoff   int64
	sent     int64
	switched bool
	prelude  []string
}

func TestHarnessModel_ChatSwitchLeavesOldTailUnread(t *testing.T) {
	for _, point := range []string{"after send", "after mark"} {
		for _, cutoff := range []int64{1, 99, 100, 101, 124} {
			t.Run(fmt.Sprintf("%s_%d", point, cutoff), func(t *testing.T) {
				model := &switchDeliveryModel{cutoff: cutoff}
				fake := newChatFake()
				fake.focus = 12
				for id := int64(1); id <= switchTailSize; id++ {
					fake.rows[12] = append(
						fake.rows[12],
						ChatRow{ID: id, Type: "message", Content: fmt.Sprintf("A%d", id)},
					)
				}

				fake.rows[9] = []ChatRow{{ID: 126, Type: "message", Content: "B answer"}}
				fake.sessions = []ChatSession{
					{ID: 12, Name: "A", Unread: switchTailSize - cutoff, LatestUnread: "A125", LatestUnreadID: 125},
					{ID: 9, Name: "B", Unread: 1, LatestUnread: "B answer", LatestUnreadID: 126},
				}
				fake.onSend = func(text string) { model.acceptSend(t, text) }
				fake.onBoundary = func(boundary string) {
					if boundary == point && model.sent == cutoff && !model.switched {
						model.commitSwitch(t)
						fake.focus = 9
					}
				}
				w := NewChat(fake, fake).(*chatLoop)

				require.Zero(t, w.pass(t.Context()))
				require.True(t, model.switched)
				assert.Empty(t, model.prelude)
				assert.Equal(t, cutoff, fake.seen[12])
				assert.Zero(t, fake.seen[9])
				require.Zero(t, w.pass(t.Context()))
				assert.Equal(t, cutoff, fake.seen[12])
				assert.Equal(t, int64(126), fake.seen[9])
				assert.Equal(t, []string{
					"→ #9 B", "#9 B\nB answer",
				}, model.prelude)
			})
		}
	}
}

func TestHarnessModel_ChatKillClosePrecedesResetAtEveryPassBoundary(t *testing.T) {
	for fenceAt := 0; fenceAt <= killPassBoundaryLimit; fenceAt++ {
		for killAt := fenceAt; killAt <= killPassBoundaryLimit; killAt++ {
			t.Run(fmt.Sprintf("fence_%d_kill_%d", fenceAt, killAt), func(t *testing.T) {
				runChatKillInterleaving(t, fenceAt, killAt)
			})
		}
	}
}

func TestChatLoop_KillResetCASPreservesNewSelection(t *testing.T) {
	fake := newChatFake()
	fake.focus = 12
	fake.killed[12] = true
	fake.rows[12] = []ChatRow{{ID: 1, Type: "session_closed", Content: "Session killed."}}
	fake.onBoundary = func(point string) {
		if point == "before reset" {
			fake.focus = 9
		}
	}
	w := NewChat(fake, fake).(*chatLoop)
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, int64(9), fake.focus)
	assert.Equal(t, []string{"#12 project - 12\nSession killed."}, fake.sent)
	assert.Equal(t, int64(1), fake.seen[12])
}

func (m *killDeliveryModel) commitFence() { m.terminating = true }

func (m *killDeliveryModel) commitKill(t *testing.T) {
	t.Helper()
	require.True(t, m.terminating)
	m.killed = true
}

func (m *killDeliveryModel) acceptSend(t *testing.T, text string) {
	t.Helper()

	if m.reset {
		expected := []string{"→ #0 Assistant", "assistant context"}
		require.Less(t, len(m.prelude), len(expected))
		require.Equal(t, expected[len(m.prelude)], text)
		m.prelude = append(m.prelude, text)
		return
	}

	expected := []string{"#12 project - 12\nanswer"}
	if m.terminating {
		expected = append(expected, "#12 project - 12\nStopping session...")
	}

	if m.killed {
		expected = append(expected, "#12 project - 12\nSession killed.")
	}

	require.Less(t, len(m.delivered), len(expected), "unexpected send after obligations drained")
	require.Equal(t, expected[len(m.delivered)], text)
	m.delivered = append(m.delivered, text)
}

func (m *killDeliveryModel) acceptReset(t *testing.T, watermark int64) {
	t.Helper()
	require.True(t, m.killed, "the terminating fence is insufficient for reset")
	require.Equal(t, []string{
		"#12 project - 12\nanswer", "#12 project - 12\nStopping session...", "#12 project - 12\nSession killed.",
	}, m.delivered)
	require.Equal(t, int64(3), watermark, "close must be marked before reset")
	m.reset = true
}

func (m *switchDeliveryModel) commitSwitch(t *testing.T) {
	t.Helper()
	require.Equal(t, m.cutoff, m.sent)
	m.switched = true
}

func (m *switchDeliveryModel) acceptSend(t *testing.T, text string) {
	t.Helper()

	if !m.switched {
		require.Less(t, m.sent, m.cutoff, "old-session sends must stop at the switch")
		m.sent++
		require.Equal(t, fmt.Sprintf("#12 A\nA%d", m.sent), text)
		return
	}

	expected := []string{"→ #9 B", "#9 B\nB answer"}
	require.Less(t, len(m.prelude), len(expected), "unexpected send after the new tail")
	require.Equal(t, expected[len(m.prelude)], text, "a work-session switch must send only its own tail")
	m.prelude = append(m.prelude, text)
}

func runChatKillInterleaving(t *testing.T, fenceAt, killAt int) {
	t.Helper()

	model := &killDeliveryModel{}
	fake := newChatFake()
	fake.focus = 12
	fake.rows[12] = []ChatRow{{ID: 1, Type: "message", Content: "answer"}}
	fake.rows[9] = []ChatRow{{ID: 50, Type: "message", Content: "never read"}}
	fake.rows[0] = []ChatRow{{ID: 40, Type: "message", Content: "assistant context"}}
	fake.seen[0] = 40
	fake.sessions = []ChatSession{{ID: 0, Name: "Assistant"}}
	w := NewChat(fake, fake).(*chatLoop)
	boundary := 0
	commitFence := func() {
		model.commitFence()
		fake.rows[12] = append(fake.rows[12], ChatRow{
			ID: 2, Type: "message", Content: "Stopping session...",
		})
	}
	commitKill := func() {
		model.commitKill(t)
		fake.killed[12] = true
		fake.rows[12] = append(fake.rows[12], ChatRow{ID: 3, Type: "session_closed", Content: "Session killed."})
	}
	fake.onBoundary = func(point string) {
		if boundary == fenceAt {
			commitFence()
		}

		if boundary == killAt {
			commitKill()
		}

		if point == "before reset" {
			model.acceptReset(t, fake.seen[12])
		}

		boundary++
	}
	fake.onSend = func(text string) { model.acceptSend(t, text) }
	require.Zero(t, w.pass(t.Context()))
	fake.boundary("pass end")
	require.LessOrEqual(t, boundary, killPassBoundaryLimit, "extend the enumerated pass boundary range")
	fake.onBoundary = func(point string) {
		if point == "before reset" {
			model.acceptReset(t, fake.seen[12])
		}
	}

	if !model.terminating {
		commitFence()
	}

	if !model.killed {
		commitKill()
	}

	for pass := 0; fake.focus != 0 && pass < 3; pass++ {
		require.Zero(t, w.pass(t.Context()))
	}

	require.True(t, model.reset)
	require.Zero(t, fake.focus)
	require.Zero(t, w.pass(t.Context()))
	assert.Equal(t, []string{
		"#12 project - 12\nanswer", "#12 project - 12\nStopping session...",
		"#12 project - 12\nSession killed.", "→ #0 Assistant", "assistant context",
	}, fake.sent)
	assert.Equal(t, int64(3), fake.seen[12])
	assert.NotContains(t, fake.reads, int64(9))
	assert.Zero(t, fake.seen[9])
}
