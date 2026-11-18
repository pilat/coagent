package session

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/tool"
)

func TestSentMessageContentOnlyHandlesSentMessages(t *testing.T) {
	for _, notice := range []string{"", "host notice"} {
		t.Run(fmt.Sprintf("notice=%q", notice), func(t *testing.T) {
			batch := &boundaryBatch{}
			input := &sessionstore.InboxInput{
				ID: 13, Source: sessionstore.InputSourceSubagent,
				Attributes: map[string]any{sessionstore.InputAttributeSignalNotice: notice},
			}
			content := "child result\n\nunchanged bytes"
			assert.Equal(t, content, sentMessageContent(batch, input, content))
			assert.Empty(t, batch.commit.Outputs)
		})
	}
}

func TestBoundarySignalAppearsInWatcherChat(t *testing.T) {
	for _, signal := range []bool{false, true} {
		t.Run(fmt.Sprintf("signal=%t", signal), func(t *testing.T) {
			db, store, s := newBoundaryFixture(
				t,
				[]llmwire.Message{usr("history"), asst("", call("signal-sleep", tool.IDSleep))},
			)
			s.stagedCalls = map[string]string{"signal-sleep": tool.IDSleep}
			attrs := map[string]any{}
			if signal {
				attrs["signal_target_id"] = 13
				attrs["signal_notice"] = "host notice"
			}
			input, err := store.Enqueue(t.Context(), sessionstore.Input{
				SessionID: s.id, Source: sessionstore.InputSourceSubagent, Content: "host result", Attributes: attrs,
			})
			require.NoError(t, err)
			batch := s.newBoundaryBatch()
			s.interruptBoundarySleeps(batch, input.Input)
			require.Len(t, batch.commit.ToolResults, 1)
			notice := "Sleep interrupted — user sent a message."
			if signal {
				notice = "Sleep interrupted — a result from another session arrived."
			}
			assert.Equal(t, notice, batch.commit.ToolResults[0].Content)
			signalNotice(batch, input.Input)
			if signal {
				require.Len(t, batch.commit.Outputs, 1)
				assert.True(t, batch.commit.Outputs[0].PersistOnly)
			} else {
				assert.Empty(t, batch.commit.Outputs)
			}
			r := &runState{}
			_, err = s.boundaryStep(t.Context(), r)
			require.NoError(t, err)
			_, err = s.boundaryStep(t.Context(), r)
			require.NoError(t, err)
			messages, err := store.LoadActiveMessages(t.Context(), s.id)
			require.NoError(t, err)
			assert.Contains(t, messages[len(messages)-1].Content, "host result")
			outputs, err := store.ChatOutputs(t.Context(), s.id, 0, 100)
			require.NoError(t, err)
			if !signal {
				assert.Empty(t, outputs)
				return
			}
			require.Len(t, outputs, 1)
			assert.Equal(t, "host notice", outputs[0].Content)
			var key string
			require.NoError(t, db.QueryRowContext(t.Context(),
				`SELECT source_key FROM session_outbox WHERE id = ?`, outputs[0].ID).Scan(&key))
			assert.Equal(t, fmt.Sprintf("input:%d:signal", input.Input.ID), key)
		})
	}
}
