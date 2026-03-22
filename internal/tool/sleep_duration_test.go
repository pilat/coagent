package tool

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDuration(t *testing.T) {
	t.Run("go duration", func(t *testing.T) {
		dur, err := ParseDuration("2h30m")
		require.NoError(t, err)
		assert.Equal(t, 2*time.Hour+30*time.Minute, dur)
	})

	t.Run("rfc3339", func(t *testing.T) {
		future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
		dur, err := ParseDuration(future)
		require.NoError(t, err)
		assert.True(t, dur > 50*time.Minute && dur < 70*time.Minute)
	})

	t.Run("days", func(t *testing.T) {
		dur, err := ParseDuration("3d")
		require.NoError(t, err)
		assert.Equal(t, 3*24*time.Hour, dur)
	})

	t.Run("weeks", func(t *testing.T) {
		dur, err := ParseDuration("2w")
		require.NoError(t, err)
		assert.Equal(t, 14*24*time.Hour, dur)
	})

	t.Run("1d", func(t *testing.T) {
		dur, err := ParseDuration("1d")
		require.NoError(t, err)
		assert.Equal(t, 24*time.Hour, dur)
	})

	t.Run("invalid", func(t *testing.T) {
		_, err := ParseDuration("not-a-duration")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "invalid duration")
	})
}
