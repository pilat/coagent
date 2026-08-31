package session

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
	"github.com/pilat/coagent/internal/llm"
)

// A client created by Factory.Create belongs to the factory until a complete
// Service is returned. In particular, malformed persisted resume state must not
// turn an otherwise ordinary construction error into a connection leak.
func TestFactoryCreateClosesLLMClientOnBuildFailure(t *testing.T) {
	client := &mockLLMClientTracked{model: "fake-model"}
	factory := NewFactoryWithOptions(
		&config.Config{Model: "fake-model"},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
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
		nil, nil, nil, nil, nil, nil, nil, nil, nil,
	)

	_, err := factory.Create(context.Background(), CreateOptions{
		ID: 1, WorkDir: t.TempDir(), OutputEnabled: true,
	})
	require.ErrorContains(t, err, "output store is required")
}
