package sessionlifecycle

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRegistrySerializesUseWithDeletion(t *testing.T) {
	t.Parallel()

	registry := NewRegistry[*int]()
	value := 1
	_, registered := registry.Register(1, &value)
	require.True(t, registered)

	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})

	go func() {
		registry.Use(1, func(*int) {
			close(entered)
			<-release
		})
		close(done)
	}()

	<-entered
	deleteDone := make(chan bool, 1)
	go func() { deleteDone <- registry.Delete(1, &value) }()

	select {
	case <-deleteDone:
		t.Fatal("delete completed while use still owned the registry entry")
	default:
	}

	close(release)
	<-done
	assert.True(t, <-deleteDone)
}

func TestRegistryCloseRejectsRegistrationAndReturnsActiveValues(t *testing.T) {
	t.Parallel()

	registry := NewRegistry[int]()
	_, registered := registry.Register(1, 10)
	require.True(t, registered)

	assert.Equal(t, []int{10}, registry.CloseAndSnapshot())
	existing, registered := registry.Register(2, 20)
	assert.False(t, registered)
	assert.Zero(t, existing)
}
