package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/llmwire"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

// ---------------------------------------------------------------------------
// Helpers / extended mocks
// ---------------------------------------------------------------------------

func unifiedCfgWithModels(ids ...string) *config.UnifiedConfig {
	uc := &config.UnifiedConfig{
		Providers: map[string]config.ProviderEntry{
			"openrouter": {Driver: "openai", APIKey: "sk-test", BaseURL: "https://openrouter.ai/api/v1"},
		},
	}
	for _, id := range ids {
		uc.Models = append(uc.Models, config.ModelEntry{
			ID: id, Name: id, Provider: "openrouter",
			EffortLevels: []string{"low", "medium", "high"}, DefaultEffort: "medium",
		})
	}
	return uc
}

// unifiedCfgWithNarrowModel mirrors a catalog that names only some of the levels.
func unifiedCfgWithNarrowModel(id string, levels []string, def string) *config.UnifiedConfig {
	return &config.UnifiedConfig{
		Providers: map[string]config.ProviderEntry{
			"openrouter": {Driver: "openai", APIKey: "sk-test", BaseURL: "https://openrouter.ai/api/v1"},
		},
		Models: []config.ModelEntry{
			{ID: id, Name: id, Provider: "openrouter", EffortLevels: levels, DefaultEffort: def},
		},
	}
}

// mockLLMClientTracked extends mockLLMClient with close tracking and reasoning level.
type mockLLMClientTracked struct {
	closed         bool
	reasoningLevel string
	model          string
}

func (m *mockLLMClientTracked) Chat(
	_ context.Context,
	_ string,
	_ []llmwire.Message,
	_ []llmwire.ToolSchema,
	_ ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	return &llmwire.Response{Text: "done"}, nil
}

func (m *mockLLMClientTracked) Model() string                  { return m.model }
func (m *mockLLMClientTracked) APIKey() string                 { return "key" }
func (m *mockLLMClientTracked) Provider() string               { return testMockModel }
func (m *mockLLMClientTracked) ContextWindow() int             { return 0 }
func (m *mockLLMClientTracked) Close() error                   { m.closed = true; return nil }
func (m *mockLLMClientTracked) GetReasoningLevel() string      { return m.reasoningLevel }
func (m *mockLLMClientTracked) SetReasoningLevel(level string) { m.reasoningLevel = level }
func (m *mockLLMClientTracked) SetSessionID(id string)         {}

type blockingLLMClient struct {
	mockLLMClientTracked
	started chan struct{}
	release chan struct{}
	closed  chan struct{}
}

func (m *blockingLLMClient) Chat(
	_ context.Context,
	_ string,
	_ []llmwire.Message,
	_ []llmwire.ToolSchema,
	_ ...llmwire.ChatOption,
) (*llmwire.Response, error) {
	close(m.started)
	<-m.release

	return &llmwire.Response{Text: "done"}, nil
}

func (m *blockingLLMClient) Close() error {
	close(m.closed)
	return nil
}

// ---------------------------------------------------------------------------
// validateModelSwitch
// ---------------------------------------------------------------------------

func TestValidateModelSwitch_ValidModel(t *testing.T) {
	s := &svc{
		cfg: &config.Config{
			UnifiedConfig: unifiedCfgWithModels("gpt-4o", "claude-3"),
		},
	}

	reasoning, err := s.validateModelSwitch("gpt-4o", "high")
	require.NoError(t, err)
	assert.Equal(t, "high", reasoning)
}

func TestValidateModelSwitch_UnknownModel(t *testing.T) {
	s := &svc{
		cfg: &config.Config{
			UnifiedConfig: unifiedCfgWithModels("gpt-4o"),
		},
	}

	_, err := s.validateModelSwitch("does-not-exist", "medium")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown model")
}

func TestValidateModelSwitch_DefaultsReasoningToMedium(t *testing.T) {
	s := &svc{
		cfg: &config.Config{
			UnifiedConfig: unifiedCfgWithModels("gpt-4o"),
		},
	}

	reasoning, err := s.validateModelSwitch("gpt-4o", "")
	require.NoError(t, err)
	assert.Equal(t, string(llm.ReasoningMedium), reasoning)
}

func TestValidateModelSwitch_InvalidReasoningLevel(t *testing.T) {
	s := &svc{
		cfg: &config.Config{
			UnifiedConfig: unifiedCfgWithModels("gpt-4o"),
		},
	}

	_, err := s.validateModelSwitch("gpt-4o", "ultra")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not accept reasoning level")
}

// The vocabulary is per-model: a level another model accepts is still invalid here.
func TestValidateModelSwitch_LevelOutsideTheModelsAllowlist(t *testing.T) {
	s := &svc{
		cfg: &config.Config{
			UnifiedConfig: unifiedCfgWithNarrowModel("glm", []string{"high", "xhigh"}, "xhigh"),
		},
	}

	_, err := s.validateModelSwitch("glm", "medium")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "accepts: high, xhigh")

	reasoning, err := s.validateModelSwitch("glm", "")
	require.NoError(t, err)
	assert.Equal(t, "xhigh", reasoning, "an unspecified level lands on the model's own default")
}

// A model with no effort choice carries no level at all, rather than a medium
// nobody honours.
func TestValidateModelSwitch_ModelWithoutEffortCarriesNoLevel(t *testing.T) {
	s := &svc{
		cfg: &config.Config{
			UnifiedConfig: unifiedCfgWithNarrowModel("minimax", nil, ""),
		},
	}

	reasoning, err := s.validateModelSwitch("minimax", "")
	require.NoError(t, err)
	assert.Empty(t, reasoning)
}

func TestValidateModelSwitch_NoProvidersReturnsError(t *testing.T) {
	s := &svc{
		cfg: &config.Config{
			UnifiedConfig: &config.UnifiedConfig{
				Models: []config.ModelEntry{{ID: "gpt-4o", Name: "GPT-4o"}},
			},
		},
	}

	_, err := s.validateModelSwitch("gpt-4o", "medium")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no providers configured")
}

// ---------------------------------------------------------------------------
// handleSetModel
// ---------------------------------------------------------------------------

func TestHandleSetModel_SwitchesLLMClientAndClosesOld(t *testing.T) {
	oldClient := &mockLLMClientTracked{model: "old-model"}
	newClient := &mockLLMClientTracked{model: "new-model"}

	s := &svc{
		cfg: &config.Config{
			UnifiedConfig: unifiedCfgWithModels("new-model"),
		},
		llmClient: oldClient,
		model:     "old-model",
		prompt:    newPromptBuilder("", "", ""),
		ms:        newMessageStore(nil, 0),
		newLLMWithModel: func(_ *config.Config, _ string) (llm.Client, error) {
			return newClient, nil
		},
	}

	err := s.handleSetModel("new-model", "medium")
	require.NoError(t, err)

	assert.True(t, oldClient.closed, "old LLM client should be closed")
	assert.Equal(t, newClient, s.llmClient, "session should hold the new client")
}

func TestHandleSetModel_UpdatesModelField(t *testing.T) {
	newClient := &mockLLMClientTracked{model: "claude-3"}

	s := &svc{
		cfg: &config.Config{
			UnifiedConfig: unifiedCfgWithModels("claude-3"),
		},
		llmClient: &mockLLMClientTracked{model: "old"},
		model:     "old",
		prompt:    newPromptBuilder("", "", ""),
		ms:        newMessageStore(nil, 0),
		newLLMWithModel: func(_ *config.Config, _ string) (llm.Client, error) {
			return newClient, nil
		},
	}

	require.NoError(t, s.handleSetModel("claude-3", "low"))

	assert.Equal(t, "claude-3", s.model)
	assert.Equal(t, "low", s.reasoningLevel)
}

func TestHandleSetModel_SetsReasoningLevelOnNewClient(t *testing.T) {
	newClient := &mockLLMClientTracked{model: "gpt-4o"}

	s := &svc{
		cfg: &config.Config{
			UnifiedConfig: unifiedCfgWithModels("gpt-4o"),
		},
		llmClient: &mockLLMClientTracked{model: "old"},
		prompt:    newPromptBuilder("", "", ""),
		ms:        newMessageStore(nil, 0),
		newLLMWithModel: func(_ *config.Config, _ string) (llm.Client, error) {
			return newClient, nil
		},
	}

	require.NoError(t, s.handleSetModel("gpt-4o", "high"))

	assert.Equal(t, "high", newClient.reasoningLevel)
}

func TestHandleSetModel_FactoryErrorPropagates(t *testing.T) {
	factoryErr := errors.New("factory boom")

	s := &svc{
		cfg: &config.Config{
			UnifiedConfig: unifiedCfgWithModels("gpt-4o"),
		},
		llmClient: &mockLLMClientTracked{model: "old"},
		ms:        newMessageStore(nil, 0),
		newLLMWithModel: func(_ *config.Config, _ string) (llm.Client, error) {
			return nil, factoryErr
		},
	}

	err := s.handleSetModel("gpt-4o", "medium")
	require.Error(t, err)
	assert.ErrorIs(t, err, factoryErr)
}

// A /model switch on the daemon goroutine mutates the model triplet while the loop
// reads it (callLLM / buildSessionStatus). modelMu must make that race-free — run
// under -race to catch a regression.
func TestHandleSetModel_RaceWithLoopRead(t *testing.T) {
	s := &svc{
		cfg:            &config.Config{UnifiedConfig: unifiedCfgWithModels("m1", "m2")},
		llmClient:      &mockLLMClientTracked{model: "m1"},
		model:          "m1",
		reasoningLevel: "medium",
		prompt:         newPromptBuilder("", "", ""),
		registry:       tool.NewRegistry(),
		ms:             newMessageStore(nil, 0),
		newLLMWithModel: func(_ *config.Config, id string) (llm.Client, error) {
			return &mockLLMClientTracked{model: id}, nil
		},
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	var setErr error

	wg.Go(func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = s.currentLLM()
				_ = s.prompt.systemPrompt()
				_ = s.buildSessionStatus(context.Background())
			}
		}
	})

	wg.Go(func() {
		defer close(stop)

		models := []string{"m1", "m2"}
		for i := range 200 {
			if err := s.handleSetModel(models[i%2], "medium"); err != nil {
				setErr = err
				return
			}
		}
	})

	wg.Wait()
	require.NoError(t, setErr)
}

func TestHandleSetModelWaitsForInFlightChatBeforeClosingOldClient(t *testing.T) {
	oldClient := &blockingLLMClient{
		mockLLMClientTracked: mockLLMClientTracked{model: "m1"},
		started:              make(chan struct{}),
		release:              make(chan struct{}),
		closed:               make(chan struct{}),
	}
	newClientBuilt := make(chan struct{})
	s := &svc{
		cfg:            &config.Config{UnifiedConfig: unifiedCfgWithModels("m1", "m2")},
		llmClient:      oldClient,
		model:          "m1",
		reasoningLevel: "medium",
		prompt:         newPromptBuilder("", "", ""),
		newLLMWithModel: func(_ *config.Config, id string) (llm.Client, error) {
			close(newClientBuilt)
			return &mockLLMClientTracked{model: id}, nil
		},
	}

	chatDone := make(chan error, 1)
	go func() {
		_, err := s.chat(context.Background(), "system", nil, nil)
		chatDone <- err
	}()
	<-oldClient.started

	switchDone := make(chan error, 1)
	go func() { switchDone <- s.handleSetModel("m2", "medium") }()
	<-newClientBuilt

	select {
	case <-oldClient.closed:
		t.Fatal("old client closed while its Chat was still in flight")
	case err := <-switchDone:
		t.Fatalf("model switch returned before in-flight Chat: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(oldClient.release)
	require.NoError(t, <-chatDone)
	require.NoError(t, <-switchDone)

	select {
	case <-oldClient.closed:
	case <-time.After(time.Second):
		t.Fatal("old client was not closed after in-flight Chat returned")
	}
}

func TestBuildModelsSection_OnlyNamesCurrentModel(t *testing.T) {
	section := buildModelsSection("some-model")

	assert.Equal(t, "\n- Model: some-model", section)
}

func TestHandleSetModel_DoesNotExposeOtherConfiguredModels(t *testing.T) {
	uc := unifiedCfgWithModels("new-model")
	uc.Models[0].ContextWindow = 100_000
	uc.Models = append(uc.Models, config.ModelEntry{
		ID: "hidden-model", Name: "Hidden", Provider: "openrouter", ContextWindow: 200_000,
	})

	s := &svc{
		cfg:       &config.Config{UnifiedConfig: uc},
		llmClient: &mockLLMClientTracked{model: "old-model"},
		model:     "old-model",
		prompt:    newPromptBuilder("", "", buildModelsSection("old-model")),
		ms:        newMessageStore(nil, 0),
		newLLMWithModel: func(_ *config.Config, _ string) (llm.Client, error) {
			return &mockLLMClientTracked{model: "new-model"}, nil
		},
	}

	require.NoError(t, s.handleSetModel("new-model", ""))

	prompt := s.prompt.systemPrompt()
	assert.Contains(t, prompt, "- Model: new-model")
	assert.NotContains(t, prompt, "hidden-model")
	assert.NotContains(t, prompt, "context")
}

// mockLLMWithSessionTracking tracks SetSessionID calls for model switch tests.
type mockLLMWithSessionTracking struct {
	mockLLMClientTracked
	sessionID string
}

func (m *mockLLMWithSessionTracking) SetSessionID(id string) { m.sessionID = id }

func TestHandleSetModel_PreservesSessionID(t *testing.T) {
	var newClient *mockLLMWithSessionTracking

	s := &svc{
		cfg: &config.Config{
			UnifiedConfig: unifiedCfgWithModels("new-model"),
		},
		llmClient: &mockLLMClientTracked{model: "old-model"},
		id:        42,
		rootID:    42,
		model:     "old-model",
		prompt:    newPromptBuilder("", "", ""),
		ms:        newMessageStore(nil, 0),
		newLLMWithModel: func(_ *config.Config, _ string) (llm.Client, error) {
			newClient = &mockLLMWithSessionTracking{mockLLMClientTracked: mockLLMClientTracked{model: "new-model"}}
			return newClient, nil
		},
	}

	err := s.handleSetModel("new-model", "medium")
	require.NoError(t, err)

	require.NotNil(t, newClient)
	assert.Equal(t, "42", newClient.sessionID, "model switch must preserve session ID")
}

func TestHandleSetModel_PreservesSubagentSessionID(t *testing.T) {
	var newClient *mockLLMWithSessionTracking

	s := &svc{
		cfg: &config.Config{
			UnifiedConfig: unifiedCfgWithModels("new-model"),
		},
		llmClient: &mockLLMClientTracked{model: "old-model"},
		id:        99,
		rootID:    1, // different from id = subagent
		model:     "old-model",
		prompt:    newPromptBuilder("", "", ""),
		ms:        newMessageStore(nil, 0),
		newLLMWithModel: func(_ *config.Config, _ string) (llm.Client, error) {
			newClient = &mockLLMWithSessionTracking{mockLLMClientTracked: mockLLMClientTracked{model: "new-model"}}
			return newClient, nil
		},
	}

	err := s.handleSetModel("new-model", "medium")
	require.NoError(t, err)

	require.NotNil(t, newClient)
	assert.Equal(t, "1:99", newClient.sessionID, "model switch in subagent must use {rootID}:{id}")
}

// The session's reasoning level must reach the freshly built client, not just the
// DB record — before this, only SetSessionID was applied at build time.
func TestNewWithOptions_AppliesReasoningLevelToClient(t *testing.T) {
	tests := []struct {
		name  string
		level string
		want  string
	}{
		{name: "explicit level", level: "high", want: "high"},
		{name: "unset falls back to medium", level: "", want: string(llm.ReasoningMedium)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &mockLLMClientTracked{model: testMockModel}

			_, err := newWithOptions(context.Background(), params{
				Config:    &config.Config{WorkDir: t.TempDir(), Model: testMockModel},
				LLMClient: client,
				TodoStore: todo.New(),
				Loader:    loader.New(),
				Registry:  tool.NewRegistry(),
			}, options{ID: 1, ReasoningLevel: tt.level})
			require.NoError(t, err)

			assert.Equal(t, tt.want, client.GetReasoningLevel())
		})
	}
}
