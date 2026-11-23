package localapi

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/pilat/coagent/api/coagent/v1"
	"github.com/pilat/coagent/api/coagent/v1/coagentv1connect"
)

func TestSendInput_OutlivesItsClientAndEndsWithTheServer(t *testing.T) {
	t.Parallel()
	f := newAPIFixture(t)
	h := f.start(t)
	id := f.root(t, h, "alpha")
	client := coagentv1connect.NewSessionServiceClient(h.client(t, "Bearer "+f.tokens["alpha"]))
	for _, tc := range []struct {
		name    string
		closing bool
	}{
		{name: "client deadline"},
		{name: "server close", closing: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan context.Context, 1)
			finished := make(chan error, 1)
			release := make(chan struct{}, 1)
			defer close(release)
			f.backend.mu.Lock()
			f.backend.send = func(ctx context.Context) error {
				started <- ctx
				var err error
				select {
				case <-release:
				case <-ctx.Done():
					err = ctx.Err()
				}
				finished <- err
				return err
			}
			f.backend.mu.Unlock()
			timeout := 300 * time.Millisecond
			if tc.closing {
				timeout = 5 * time.Second
			}
			request, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			result := make(chan error, 1)
			go func() {
				_, err := client.SendInput(request, &coagentv1.SendInputRequest{
					SessionId: id, Text: "/kill", IdempotencyKey: t.Name(),
				})
				result <- err
			}()
			var sendCtx context.Context
			select {
			case sendCtx = <-started:
			case <-time.After(2 * time.Second):
				t.Fatal("send never reached the backend")
			}
			author, err := clientFrom(sendCtx)
			require.NoError(t, err)
			assert.NotNil(t, author, "accepted send must retain request values")
			if tc.closing {
				h.close(t)
				require.ErrorIs(t, sendCtx.Err(), context.Canceled)
			} else {
				select {
				case err := <-result:
					require.ErrorIs(t, err, context.DeadlineExceeded)
				case <-time.After(2 * time.Second):
					t.Fatal("client deadline did not end the call")
				}
				require.NoError(t, sendCtx.Err(), "client deadline cancelled the accepted send")
				_, hasDeadline := sendCtx.Deadline()
				assert.False(t, hasDeadline, "accepted send must outlive the client deadline")
			}
			if !tc.closing {
				f.backend.mu.Lock()
				f.backend.send = nil
				f.backend.mu.Unlock()
				release <- struct{}{}
			}
			select {
			case err := <-finished:
				if tc.closing {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					require.NoError(t, err)
				}
			case <-time.After(time.Second):
				t.Fatal("backend did not finish the accepted send")
			}
		})
	}
}
