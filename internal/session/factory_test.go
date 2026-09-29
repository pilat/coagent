package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm"
	"github.com/pilat/coagent/internal/sessioncalls"
	"github.com/pilat/coagent/internal/tool/builtin"
)

// A client created by Factory.Create belongs to the factory until a complete
// Service is returned. In particular, malformed persisted resume state must not
// turn an otherwise ordinary construction error into a connection leak.
func TestFactoryCreateClosesLLMClientOnBuildFailure(t *testing.T) {
	_, store, _ := newFinalOutputStore(t)
	client := &mockLLMClientTracked{model: "fake-model"}
	factory := NewFactoryWithOptions(
		&config.Config{Model: "fake-model"},
		nil, nil, store, nil, nil, nil, nil, builtin.NewResources(),
		WithLLMClientFactory(func(*config.Config) (llm.Client, error) { return client, nil }),
	)

	_, err := factory.Create(context.Background(), CreateOptions{
		ID:        1,
		WorkDir:   t.TempDir(),
		TodoItems: "not-json",
	})
	require.ErrorContains(t, err, "unmarshal todo items")
	assert.True(t, client.closed, "factory must release the client when it cannot return a session")
}

func TestFactoryCreateRequiresOutputStoreForManagedRoot(t *testing.T) {
	t.Parallel()

	factory := NewFactoryWithOptions(
		&config.Config{Model: "fake-model"},
		nil, nil, nil, nil, nil, nil, nil, builtin.NewResources(),
	)

	_, err := factory.Create(context.Background(), CreateOptions{
		ID: 1, WorkDir: t.TempDir(), OutputEnabled: true,
	})
	require.ErrorContains(t, err, "output store is required")
}

func TestOpenStoredCallsNeedsNoModelOrTools(t *testing.T) {
	_, store, sessionID := newFinalOutputStore(t)
	sess, err := sessioncalls.OpenStored(t.Context(), store, sessionID, nil)
	require.NoError(t, err)
	assert.Empty(t, sess.PendingExternalCalls())
	require.NoError(t, sess.SettleStoppedCalls(t.Context(), "stopped"))
}
