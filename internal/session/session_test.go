package session

import (
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

func TestSwitchModel_AdoptionAndRetirement(t *testing.T) {
	for _, tc := range []struct {
		name       string
		rootID, id int64
		sessionID  string
	}{{"root", 7, 7, "7"}, {"child", 7, 9, "7:9"}} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestAgent()
			s.rootID, s.id = tc.rootID, tc.id
			old := &trackedClient{}
			s.llmClient = old
			s.storeContextBaseline(1000, 1, s.modelGeneration())
			staleEpoch := s.modelGeneration()
			first := &trackedClient{}
			s.SwitchModel(
				first,
				sessionprompt.ModelSection{ID: "first", Text: sessionprompt.BuildModelsSection("first")},
			)
			assert.Same(t, old, s.currentLLM())
			assert.Zero(t, old.closed)
			latest := &trackedClient{}
			s.SwitchModel(
				latest,
				sessionprompt.ModelSection{
					ID:           "latest",
					Text:         sessionprompt.BuildModelsSection("latest"),
					NativeSearch: true,
				},
			)
			assert.Equal(t, 1, first.closed, "superseded queued client is released")
			s.registry.Register(&stubTool{id: "read"})
			s.applyModelSwitch()
			assert.Same(t, latest, s.currentLLM())
			assert.Equal(t, 1, old.closed)
			assert.Equal(t, tc.sessionID, latest.sessionID)
			assert.Contains(t, s.prompt.SystemPrompt(), "Model: latest")
			assert.NotContains(t, s.prompt.SystemPrompt(), "Model: first")
			assert.Contains(t, s.prompt.SystemPrompt(), "provided natively")
			assert.Nil(t, s.loadContextBaseline())
			_, kept := s.storeContextBaseline(5000, 1, staleEpoch)
			assert.False(t, kept)
			s.Close()
			assert.Equal(t, 1, latest.closed)
			rejected := &trackedClient{}
			s.SwitchModel(rejected, sessionprompt.ModelSection{ID: "after-close"})
			assert.Equal(t, 1, rejected.closed)
		})
	}
}

func TestNew_PersistedContextBaseline(t *testing.T) {
	for _, tc := range []struct {
		name, baselineModel string
		measured            bool
	}{{"same model", "model", true}, {"different model", "old-model", false}} {
		t.Run(tc.name, func(t *testing.T) {
			_, store, id := newAttachmentsStore(t)
			_, err := store.Commit(
				t.Context(),
				sessionstore.Commit{
					SessionID: id,
					State: sessionstore.StatePatch{
						ContextBaseline: &sessionstore.ContextBaseline{
							Model:        tc.baselineModel,
							PromptTokens: 150000,
							MessageCount: 2,
						},
					},
				},
			)
			require.NoError(t, err)
			record, err := store.GetSession(t.Context(), id)
			require.NoError(t, err)
			prompt := sessionprompt.NewBuilder("", "")
			prompt.Todos = todo.New()
			s, err := New(
				t.Context(),
				Input{
					Record:   record,
					Client:   &trackedClient{},
					Loader:   loader.New(),
					Registry: tool.NewRegistry(),
					Prompt:   prompt,
					Store:    store,
				},
			)
			require.NoError(t, err)
			t.Cleanup(s.Close)
			assert.Equal(t, tc.measured, s.loadContextBaseline() != nil)
		})
	}
}

func TestContextBaseline_CompactionClearsPersistedState(t *testing.T) {
	_, store, id := newAttachmentsStore(t)
	s := newCompactionTestSvc(&compactionMockLLM{response: textResponse(validSummary), contextWindow: 32000})
	s.id, s.rootID = id, id
	s.store = store
	s.ms = newMessageStore(store, id)
	setTestMessages(s, oversizedTranscript(32000))
	_, err := store.Commit(
		t.Context(),
		sessionstore.Commit{
			SessionID: id,
			State: sessionstore.StatePatch{
				ContextBaseline: &sessionstore.ContextBaseline{Model: "model", PromptTokens: 150000, MessageCount: 2},
			},
		},
	)
	require.NoError(t, err)
	s.installPersistedBaseline(&sessionstore.ContextBaseline{Model: s.model, PromptTokens: 150000, MessageCount: 2})
	ok, err := s.compact(t.Context(), nil)
	require.NoError(t, err)
	require.True(t, ok)
	record, err := store.GetSession(t.Context(), id)
	require.NoError(t, err)
	assert.Nil(t, record.ContextBaseline())
	rows, err := store.LoadActiveMessages(t.Context(), id)
	require.NoError(t, err)
	messages := s.ms.getMessages()
	require.Len(t, rows, len(messages))
	assert.Equal(t, llmwire.RoleUser, rows[2].Role)
}

func TestSwitchModel_PreservesReasoningEnvelope(t *testing.T) {
	s := newTestAgent(&stubTool{id: "read", result: "body"})
	next := &reasoningSwitchClient{response: textResponse("done")}
	s.llmClient = &loopScriptLLM{onCall: func(callNumber int, _ []llmwire.Message) (*llmwire.Response, error) {
		require.Equal(t, 1, callNumber)
		s.SwitchModel(next, sessionprompt.ModelSection{ID: "next", Text: sessionprompt.BuildModelsSection("next")})
		return &llmwire.Response{
			FinishType:   llmwire.FinishToolCalls,
			ToolCalls:    []llmwire.ToolCall{{ID: "read-1", Name: "read", Arguments: []byte(`{}`)}},
			ReasoningRaw: reasoningBlob,
		}, nil
	}}
	result, err := runTestLoop(t, s)
	require.NoError(t, err)
	assert.Equal(t, "done", result.Final)
	require.NotEmpty(t, next.requests)
	var assistant *llmwire.Message
	for i := range next.requests[0] {
		row := &next.requests[0][i]
		if row.Role == llmwire.RoleAssistant {
			assistant = row
			break
		}
	}
	require.NotNil(t, assistant)
	assert.JSONEq(t, string(reasoningBlob), string(assistant.ReasoningRaw))
}

func TestPrepareUserMessageExpandsDirectSkillInvocation(t *testing.T) {
	ldr := loader.New()
	ldr.RegisterSkill(&loader.Skill{
		Name:                   "review",
		Description:            "Review changes",
		DisableModelInvocation: true,
		Content:                "Review $ARGUMENTS carefully.",
	})

	s := newTestAgent()
	s.loader = ldr

	result, err := s.PrepareUserMessage("/skill review current diff")
	require.NoError(t, err)
	assert.Contains(t, result, "<skill>\n<name>review</name>")
	assert.Contains(t, result, "Review current diff carefully.")
}

func TestPrepareUserMessageRejectsNonUserInvocableSkill(t *testing.T) {
	userDisabled := false
	ldr := loader.New()
	ldr.RegisterSkill(&loader.Skill{Name: "hidden", UserInvocable: &userDisabled, Content: "hidden"})

	s := newTestAgent()
	s.loader = ldr
	_, err := s.PrepareUserMessage("/skill hidden")
	require.ErrorContains(t, err, "skill unavailable: hidden")
}
