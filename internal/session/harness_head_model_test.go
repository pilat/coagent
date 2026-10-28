package session

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/transcript"
)

type headProtocolModel struct {
	active               []llmwire.Message
	pending              []string
	started, toolPending bool
}

func TestHarnessModel_HeadBoundaryAcrossRestart(t *testing.T) {
	_, store, s := newHeadFixture(t, []llmwire.Message{usr("previous request"), asst("previous answer")})
	model := &headProtocolModel{active: []llmwire.Message{usr("previous request"), asst("previous answer")}}
	run := &runState{}
	trace := []struct{ command, text string }{
		{"input", "first request"},
		{"boundary", ""},
		{"input", "followup"},
		{"boundary", ""},
		{"restart", ""},
		{"boundary", ""},
		{"input", "new activation"},
		{"boundary", ""},
		{"call", ""},
		{"input", "queued while working"},
		{"boundary", ""},
		{"restart", ""},
		{"boundary", ""},
		{"result", ""},
		{"boundary", ""},
		{"input", "last followup"},
		{"boundary", ""},
		{"restart", ""},
		{"boundary", ""},
	}
	for index, step := range trace {
		model.apply(step.command, step.text)
		switch step.command {
		case "input":
			_, err := store.Enqueue(t.Context(), sessionstore.Input{
				SessionID: s.id, Source: sessionstore.InputSourceUser, Content: step.text,
			})
			require.NoError(t, err)
		case "boundary":
			_, err := s.boundaryStep(t.Context(), run)
			require.NoError(t, err)
		case "restart":
			s.Close()
			s = headFixtureSession(t, store, s.id)
			run = &runState{}
		case "call":
			message := asst("", call("read-pending", "read"))
			row, err := storedMessage(&message)
			require.NoError(t, err)
			_, err = s.commit(t.Context(), sessionstore.Commit{SessionID: s.id, Messages: []*transcript.Message{row}})
			require.NoError(t, err)
		case "result":
			_, err := s.commit(t.Context(), sessionstore.Commit{SessionID: s.id, ToolResults: []*transcript.Message{{
				Role: llmwire.RoleTool, ToolName: "read", ToolCallID: "read-pending", Content: "read result",
			}}})
			require.NoError(t, err)
		default:
			t.Fatalf("unknown command %q", step.command)
		}
		rows, err := store.LoadActiveMessages(t.Context(), s.id)
		require.NoError(t, err)
		require.Len(t, rows, len(model.active), "step %d: %s", index, step.command)
		for i, expected := range model.active {
			assert.Equal(t, expected.Role, rows[i].Role, "step %d, row %d", index, i)
			assert.Contains(t, rows[i].Content, expected.Content, "step %d, row %d", index, i)
		}
		pending, err := store.ListPending(t.Context(), s.id)
		require.NoError(t, err)
		assert.Len(t, pending, len(model.pending), "step %d: %s", index, step.command)
	}
}

func (m *headProtocolModel) apply(command, text string) {
	switch command {
	case "input":
		m.pending = append(m.pending, text)
	case "boundary":
		if m.toolPending || len(m.pending) == 0 {
			return
		}
		if !m.started {
			m.active = []llmwire.Message{usr("fresh desk")}
		}
		for _, input := range m.pending {
			m.active = append(m.active, usr(input))
		}
		m.pending = nil
		m.started = true
	case "restart":
		m.started = false
	case "call":
		m.toolPending = true
		m.active = append(m.active, asst(""))
	case "result":
		m.toolPending = false
		m.active = append(m.active, llmwire.Message{Role: llmwire.RoleTool, Content: "read result"})
	}
}
