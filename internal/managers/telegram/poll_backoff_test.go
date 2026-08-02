package telegram

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func TestNextPollWait(t *testing.T) {
	log := zap.NewNop()

	t.Run("transient backs off exponentially, capped", func(t *testing.T) {
		backoff := reconnectBackoffBase
		warned := false
		err := errors.New("dial tcp: timeout")

		waits := []time.Duration{}
		for range 6 {
			waits = append(waits, nextPollWait(err, &backoff, &warned, log))
		}

		assert.Equal(t, []time.Duration{3, 6, 12, 24, 48, 60}, toSeconds(waits))
		assert.False(t, warned)
	})

	t.Run("429 honored to the second, backoff untouched", func(t *testing.T) {
		backoff := 12 * time.Second
		warned := false
		err := &tgAPIError{Method: "getUpdates", ErrorCode: 429, RetryAfter: 7}

		got := nextPollWait(err, &backoff, &warned, log)

		assert.Equal(t, 7*time.Second, got)
		assert.Equal(t, 12*time.Second, backoff, "429 must not advance the exponential backoff")
	})

	t.Run("fatal auth error throttles to max and warns once", func(t *testing.T) {
		backoff := reconnectBackoffBase
		warned := false
		err := &tgAPIError{Method: "getUpdates", ErrorCode: 401, Description: "Unauthorized"}

		got := nextPollWait(err, &backoff, &warned, log)

		assert.Equal(t, reconnectBackoffMax, got)
		assert.True(t, warned)
	})

	t.Run("conflicting poller is fatal", func(t *testing.T) {
		backoff := reconnectBackoffBase
		warned := false
		err := &tgAPIError{Method: "getUpdates", ErrorCode: 409}

		assert.Equal(t, reconnectBackoffMax, nextPollWait(err, &backoff, &warned, log))
	})

	t.Run("non-fatal api error backs off like a transient", func(t *testing.T) {
		backoff := reconnectBackoffBase
		warned := false
		err := &tgAPIError{Method: "getUpdates", ErrorCode: 500}

		assert.Equal(t, reconnectBackoffBase, nextPollWait(err, &backoff, &warned, log))
		assert.Equal(t, 6*time.Second, backoff)
	})
}

func toSeconds(ds []time.Duration) []time.Duration {
	out := make([]time.Duration, len(ds))
	for i, d := range ds {
		out[i] = d / time.Second
	}

	return out
}
