package ctl

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/internal/config"
)

// A saturated notification consumer must not stop the shared reader from
// reaching an RPC reply. Overflow is intentional and observable to the caller.
func TestClient_NotificationOverflowDoesNotBlockReplies(t *testing.T) {
	const overflow = 7

	h := newHarnessWithRegistration(t, &config.Config{}, func(server *Server) {
		require.NoError(t, server.Register("overflow", func(
			_ context.Context,
			conn *Conn,
			_ json.RawMessage,
		) (any, *Error) {
			for range notifyBuffer + overflow {
				if err := conn.Notify("chat_event", map[string]bool{"pushed": true}); err != nil {
					return nil, &Error{Code: CodeInternal, Message: err.Error()}
				}
			}

			return map[string]bool{"replied": true}, nil
		}))
	})
	c := h.dial(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var reply map[string]bool
	require.NoError(t, c.Call(ctx, "overflow", nil, &reply))
	assert.Equal(t, map[string]bool{"replied": true}, reply)
	assert.Equal(t, uint64(overflow), c.DroppedNotifications())

	pushes := drainNotifications(t, c, notifyBuffer)
	assert.Len(t, pushes, notifyBuffer)
}
