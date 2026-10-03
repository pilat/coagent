package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/sessionprompt"
	"github.com/pilat/coagent/internal/sessionstore"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

func TestRun_DurableTurns(t *testing.T) {
	tests := []struct {
		name, input string
		responses   []*llmwire.Response
		tools       []tool.Tool
		calls       int
		final       string
		status      sessionstore.SessionStatus
		wantError   bool
		ownerless   bool
	}{
		{
			name:      "confirmed answer",
			input:     "answer the request",
			responses: []*llmwire.Response{textResponse("candidate"), textResponse("confirmed")},
			calls:     2,
			final:     "candidate",
			status:    sessionstore.SessionStatusCompleted,
		},
		{
			name:      "ownerless observer answer",
			input:     "answer the request",
			responses: []*llmwire.Response{textResponse("candidate"), textResponse("confirmed")},
			calls:     2,
			final:     "candidate",
			status:    sessionstore.SessionStatusCompleted,
			ownerless: true,
		},
		{
			name:  "ordinary tool then confirmation",
			input: "read a file",
			responses: []*llmwire.Response{
				toolCallResponse("read-1", "read"),
				textResponse("candidate"),
				textResponse("confirmed"),
			},
			tools:  []tool.Tool{&stubTool{id: "read", result: "body"}},
			calls:  3,
			final:  "candidate",
			status: sessionstore.SessionStatusCompleted,
		},
		{
			name:      "owned sleep suspends",
			input:     "wait",
			responses: []*llmwire.Response{toolCallResponse("sleep-1", tool.IDSleep)},
			tools:     []tool.Tool{&stubTool{id: tool.IDSleep, err: tool.ErrSuspend}},
			calls:     1,
			status:    sessionstore.SessionStatusSuspended,
		},
		{
			name:  "empty stop breaks after six",
			input: "answer",
			responses: []*llmwire.Response{
				textResponse(""),
				textResponse(""),
				textResponse(""),
				textResponse(""),
				textResponse(""),
				textResponse(""),
			},
			calls:  6,
			status: sessionstore.SessionStatusError,
		},
		{
			name:      "unknown finish retains cost",
			input:     "answer",
			responses: []*llmwire.Response{{Text: "partial", FinishType: llmwire.FinishUnknown, CostUSD: 0.25}},
			calls:     1,
			status:    sessionstore.SessionStatusError,
			wantError: true,
		},
		{
			name:  "length retry retains rejected cost",
			input: "answer",
			responses: []*llmwire.Response{
				{Text: "partial", FinishType: llmwire.FinishLength, CostUSD: 0.25},
				textResponse("candidate"),
				textResponse("confirmed"),
			},
			calls:  3,
			final:  "candidate",
			status: sessionstore.SessionStatusCompleted,
		},
		{name: "status does not call model", input: "/status", calls: 0, status: sessionstore.SessionStatusCompleted},
		{
			name:   "unknown skill rejection does not call model",
			input:  "/skill missing",
			calls:  0,
			status: sessionstore.SessionStatusCompleted,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, store, id := newAttachmentsStore(t)
			if tc.ownerless {
				require.NoError(t, store.SetAttributes(t.Context(), id, nil))
			}
			_, err := store.Enqueue(
				t.Context(),
				sessionstore.Input{SessionID: id, Source: sessionstore.InputSourceUser, Content: tc.input},
			)
			require.NoError(t, err)
			record, err := store.GetSession(t.Context(), id)
			require.NoError(t, err)
			client := &loopScriptLLM{responses: tc.responses}
			reg := tool.NewRegistry()
			for _, tt := range tc.tools {
				reg.Register(tt)
			}
			prompt := sessionprompt.NewBuilder("stable system prompt", "")
			prompt.WorkDir = t.TempDir()
			prompt.Todos = todo.New()
			var notes []string
			s, err := New(
				t.Context(),
				Input{
					Record:         record,
					Client:         client,
					Loader:         loader.New(),
					Registry:       reg,
					Prompt:         prompt,
					Store:          store,
					Events:         noteEvents{notes: &notes},
					OpeningContext: "project instructions",
					OutputEnabled:  !tc.ownerless,
				},
			)
			require.NoError(t, err)
			t.Cleanup(s.Close)
			result, err := s.Run(t.Context())
			if tc.wantError {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.calls, client.calls)
			assert.Equal(t, tc.final, result.Final)
			record, err = store.GetSession(t.Context(), id)
			require.NoError(t, err)
			assert.Equal(t, tc.status, record.Status)
			pending, err := store.ListPending(t.Context(), id)
			require.NoError(t, err)
			assert.Empty(t, pending)
			if tc.status == sessionstore.SessionStatusSuspended {
				assert.True(t, result.Suspended)
				assert.Equal(t, []PendingToolCall{{ID: "sleep-1", Name: tool.IDSleep}}, s.PendingExternalCalls())
			}
			if tc.final != "" {
				assert.Equal(t, 1, countNotes(notes, tc.final), "candidate appears only after confirmation")
				assert.Contains(t, notes, tc.final, "live observers receive the raw answer")
				reloaded, err := New(
					t.Context(),
					Input{
						Record:        record,
						Client:        &loopScriptLLM{},
						Loader:        loader.New(),
						Registry:      reg,
						Prompt:        prompt,
						Store:         store,
						Events:        noteEvents{notes: &notes},
						OutputEnabled: !tc.ownerless,
					},
				)
				require.NoError(t, err)
				t.Cleanup(reloaded.Close)
				_, err = reloaded.Run(t.Context())
				require.NoError(t, err)
				assert.Equal(t, 1, countNotes(notes, tc.final), "reconstruction does not publish historical output")
			}
			if tc.name == "unknown finish retains cost" || tc.name == "length retry retains rejected cost" {
				_, _, cost, err := store.GetSessionTreeUsage(context.Background(), id)
				require.NoError(t, err)
				assert.InDelta(t, 0.25, cost, 0.000001)
			}
		})
	}
}

func TestLastAssistantTextOnly(t *testing.T) {
	tests := []struct {
		name     string
		messages []llmwire.Message
		want     string
	}{
		{
			name:     "empty history",
			messages: []llmwire.Message{},
			want:     "",
		},
		{
			name: "last message is assistant with no tool calls",
			messages: []llmwire.Message{
				{Role: llmwire.RoleUser, Content: "hello"},
				{Role: llmwire.RoleAssistant, Content: "how can I help?"},
			},
			want: "how can I help?",
		},
		{
			name: "last message is assistant WITH tool calls",
			messages: []llmwire.Message{
				{Role: llmwire.RoleUser, Content: "run something"},
				{
					Role:      llmwire.RoleAssistant,
					Content:   "sure",
					ToolCalls: []llmwire.ToolCall{{ID: "tc1", Name: "bash"}},
				},
			},
			want: "",
		},
		{
			name: "last message is user",
			messages: []llmwire.Message{
				{Role: llmwire.RoleAssistant, Content: "previous reply"},
				{Role: llmwire.RoleUser, Content: "follow-up question"},
			},
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := lastAssistantTextOnly(tc.messages)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestLastUserMessage(t *testing.T) {
	tests := []struct {
		name     string
		messages []llmwire.Message
		want     string
	}{
		{
			name:     "empty history",
			messages: []llmwire.Message{},
			want:     "",
		},
		{
			name: "last message is user",
			messages: []llmwire.Message{
				{Role: llmwire.RoleAssistant, Content: "previous reply"},
				{Role: llmwire.RoleUser, Content: "my latest question"},
			},
			want: "my latest question",
		},
		{
			name: "last message is assistant",
			messages: []llmwire.Message{
				{Role: llmwire.RoleUser, Content: "hello"},
				{Role: llmwire.RoleAssistant, Content: "hi there"},
			},
			want: "hello",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := lastUserMessage(tc.messages)
			assert.Equal(t, tc.want, got)
		})
	}
}
