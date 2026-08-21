package main

import (
	"context"
	"fmt"
	"time"

	"github.com/pilat/coagent/internal/ctl"
)

type chatFatalError struct {
	cause error
}

func (e *chatFatalError) Error() string { return e.cause.Error() }

// reconnect waits for the daemon to return and re-attaches to the same chat.
// Its result is shared by every path that observed the same failed client.
func (c *chat) reconnect(ctx context.Context, failed *ctl.Client) error {
	c.reconnectMu.Lock()
	defer c.reconnectMu.Unlock()

	if c.currentClient() != failed {
		return nil
	}

	if c.reconnectFailed == failed {
		return c.reconnectErr
	}

	c.println("daemon restarting…")

	_ = failed.Close()

	deadline := time.Now().Add(c.budget)
	for time.Now().Before(deadline) {
		if err := c.connect(ctx); err == nil {
			c.println("reconnected.")

			return nil
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for the daemon: %w", ctx.Err())
		case <-time.After(c.poll):
		}
	}

	err := fmt.Errorf("the daemon did not come back within %s — check `coagent status`", c.budget)
	c.reconnectFailed, c.reconnectErr = failed, err

	return err
}

func (c *chat) takeFatal() error {
	select {
	case err := <-c.fatal:
		return err
	default:
		return nil
	}
}
