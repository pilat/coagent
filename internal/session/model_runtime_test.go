package session

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/loader"
	"github.com/pilat/coagent/internal/todo"
	"github.com/pilat/coagent/internal/tool"
)

type modelAuthority struct{}

type authorizedModel struct {
	mockLLMWithSessionTracking
	authorizer llm.ImageAuthorizer
}

func (*modelAuthority) AuthorizeRead(string, string) error { return nil }

func (m *authorizedModel) SetImageAuthorizer(authorizer llm.ImageAuthorizer) {
	m.authorizer = authorizer
}

func TestModelRuntimeCloseRejectsReplacementBuiltBeforeClosure(t *testing.T) {
	oldClient := &mockLLMClientTracked{model: "m1"}
	replacement := &mockLLMClientTracked{model: "m2"}
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	factory := func(*config.Config, string) (llm.Client, error) {
		close(started)
		<-release
		return replacement, nil
	}
	prompt := newPromptBuilder("", buildModelsSection("m1"))
	model := newModelRuntime(oldClient, &config.Config{
		Model: "m1", UnifiedConfig: unifiedCfgWithModels("m1", "m2"),
	}, factory, prompt, tool.NewRegistry(), nil, nil, 1, 1)
	done := make(chan error, 1)
	go func() { done <- model.SetModel("m2", "medium") }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("replacement construction did not start")
	}

	require.NoError(t, model.Close())
	require.True(t, oldClient.closed)
	releaseOnce.Do(func() { close(release) })
	select {
	case err := <-done:
		require.ErrorIs(t, err, errModelClosed)
	case <-time.After(time.Second):
		t.Fatal("model switch did not observe closure")
	}

	assert.True(t, replacement.closed)
	assert.Equal(t, "m1", model.snapshot().model)
	assert.Contains(t, prompt.systemPrompt(), "Model: m1")
	assert.NotContains(t, prompt.systemPrompt(), "Model: m2")
	_, err := model.Chat(t.Context(), "", nil, nil)
	require.ErrorIs(t, err, errModelClosed)
	require.ErrorIs(t, model.SetModel("m2", "medium"), errModelClosed)
	require.NoError(t, model.Close())
}

func TestModelRuntimeCloseWaitsForInFlightChat(t *testing.T) {
	client := &blockingLLMClient{
		started: make(chan struct{}), release: make(chan struct{}), closed: make(chan struct{}),
	}
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(client.release) }) })
	model := newTestModelRuntime(client, nil, 0)
	chatDone := make(chan error, 1)
	go func() {
		_, err := model.Chat(context.Background(), "", nil, nil)
		chatDone <- err
	}()
	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("provider call did not start")
	}

	closeDone := make(chan error, 1)
	go func() { closeDone <- model.Close() }()
	owner := model.(*sessionModel)
	// Wait for Close to contest the lease, rather than guessing when it ran.
	require.Eventually(t, func() bool {
		select {
		case <-client.closed:
			return true
		default:
		}
		if owner.mu.TryRLock() {
			owner.mu.RUnlock()
			return false
		}
		return true
	}, time.Second, time.Millisecond)
	select {
	case <-client.closed:
		t.Fatal("client closed during provider I/O")
	default:
	}

	releaseOnce.Do(func() { close(client.release) })
	select {
	case err := <-chatDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("provider call did not return")
	}
	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("close did not finish after provider I/O")
	}
	require.NoError(t, model.Close())
}

func TestModelRuntimeConfiguresInitialAndReplacementAuthority(t *testing.T) {
	authority := &modelAuthority{}
	initial := &authorizedModel{}
	replacement := &authorizedModel{}
	factory := func(*config.Config, string) (llm.Client, error) { return replacement, nil }
	model := newModelRuntime(initial, &config.Config{
		Model: "m1", UnifiedConfig: unifiedCfgWithModels("m1", "m2"),
	}, factory, newPromptBuilder("", ""), tool.NewRegistry(), authority, nil, 9, 1)
	model.initializeClient("high")
	assert.Same(t, authority, initial.authorizer)
	assert.Equal(t, "1:9", initial.sessionID)
	assert.Equal(t, "high", initial.reasoningLevel)

	require.NoError(t, model.SetModel("m2", "low"))
	assert.Same(t, authority, replacement.authorizer)
	assert.Equal(t, "1:9", replacement.sessionID)
	assert.Equal(t, "low", replacement.reasoningLevel)
}

func TestNewWithOptionsConfiguresModelBeforeRenderingRegistry(t *testing.T) {
	client := &mockLLMWithSessionTracking{}
	reg := registryWithTools("read", "webfetch")
	service, err := newWithOptions(t.Context(), params{
		Config: &config.Config{
			WorkDir: t.TempDir(), Model: "or-model", UnifiedConfig: searchGuidanceConfig(),
		},
		LLMClient: client, Loader: loader.New(), TodoStore: todo.New(), Registry: reg,
	}, options{ID: 9, RootID: 1, ReasoningLevel: "high"})
	require.NoError(t, err)
	t.Cleanup(service.Close)
	assert.Contains(t, service.(*svc).prompt.systemPrompt(), "provided natively by your model provider")
	assert.Equal(t, "1:9", client.sessionID)
	assert.Equal(t, "high", client.reasoningLevel)
}

func TestNewWithOptionsFailedPersistenceLeavesClientConfigurationUntouched(t *testing.T) {
	client := &mockLLMWithSessionTracking{}
	service, err := newWithOptions(t.Context(), params{
		Config:    &config.Config{WorkDir: t.TempDir(), Model: "test-model"},
		LLMClient: client, Loader: loader.New(), TodoStore: todo.New(), Registry: tool.NewRegistry(),
		Store: &mockSessionStore{failCall: 1},
	}, options{ID: 9, RootID: 1, ReasoningLevel: "high"})
	require.ErrorContains(t, err, "persist initial state")
	assert.Nil(t, service)
	assert.Empty(t, client.sessionID)
	assert.Empty(t, client.reasoningLevel)
	assert.False(t, client.closed, "construction caller retains failure cleanup ownership")
}
