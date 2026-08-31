package sessionlifecycle

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRecoveryCloseCancelsAndPreventsRestart(t *testing.T) {
	t.Parallel()

	recovery := NewRecovery()
	started := make(chan struct{})
	release := make(chan struct{})
	require.True(t, recovery.Start(t.Context(), func(ctx context.Context) {
		close(started)
		<-ctx.Done()
		<-release
	}))
	<-started

	done := recovery.Close()
	require.NotNil(t, done)
	select {
	case <-done:
		t.Fatal("recovery completed before its worker returned")
	default:
	}

	close(release)
	<-done
	assert.False(t, recovery.Start(t.Context(), func(context.Context) {}))
}

func TestRecoveryCloseBeforeStartIsTerminal(t *testing.T) {
	t.Parallel()

	recovery := NewRecovery()
	assert.Nil(t, recovery.Close())
	assert.False(t, recovery.Start(t.Context(), func(context.Context) {}))
	assert.False(t, recovery.Active())
}
