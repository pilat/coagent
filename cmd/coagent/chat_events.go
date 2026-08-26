package main

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/logger"
	"github.com/pilat/coagent/internal/managers/cli"
	"github.com/pilat/coagent/internal/sessionevent"
)

// startEvents starts the one supervisor that owns every push stream this chat
// uses, including the streams acquired after a daemon restart.
func (c *chat) startEvents(ctx context.Context) {
	c.pushWG.Add(1)

	log := logger.Ctx(ctx).Named("cmd.chat.events")

	// Counted before the goroutine runs, so a caller that just started a reader
	// can be held to the one-owner rule immediately.
	if live := c.pushLive.Add(1); live > c.pushPeak.Load() {
		c.pushPeak.Store(live)
	}

	go func() {
		defer c.pushLive.Add(-1)
		defer c.pushWG.Done()
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Error("goroutine panic", zap.Any("recovered", recovered), zap.Stack("stack"))
				c.setBusy(false)

				select {
				case c.fatal <- &chatFatalError{cause: fmt.Errorf("chat event supervisor panicked: %v", recovered)}:
				default:
				}
			}
		}()

		c.superviseEvents(ctx)
	}()
}

// superviseEvents reconnects across daemon images so a config apply's accepted
// turn can receive the answer produced after its restart.
func (c *chat) superviseEvents(ctx context.Context) {
	for {
		client := c.currentClient()
		if client == nil || !c.printEvents(ctx, client) {
			return
		}

		if err := c.reconnect(ctx, client); err != nil {
			c.setBusy(false)

			select {
			case c.fatal <- &chatFatalError{cause: err}:
			default:
			}

			return
		}
	}
}

// printEvents renders one connection's push stream and reports an unexpected
// drop to the supervisor so it can reconnect without waiting for user input.
func (c *chat) printEvents(ctx context.Context, client *ctl.Client) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case n, ok := <-client.Notifications():
			if !ok {
				return ctx.Err() == nil
			}

			switch n.Method {
			case cli.EventMethod:
				c.render(n.Params)
			case cli.SecretRequestMethod:
				c.queueSecret(n.Params)
			case cli.SecretResolvedMethod:
				c.dismissSecret(n.Params)
			}
		}
	}
}

// queueSecret hands the request to the input loop: a masked read here would race
// the line reader, and blocking here would stop the push stream draining.
func (c *chat) queueSecret(params json.RawMessage) {
	var req cli.SecretRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return
	}

	if !c.acceptSecret(req) {
		return
	}

	c.enqueueSecret(req)
}

func (c *chat) enqueueSecret(req cli.SecretRequest) {
	select {
	case c.secrets <- req:
	default:
		c.forgetRequested(req.RequestID)
		c.errorf("dropped the prompt for %s: too many unanswered requests", req.Name)
	}
}

func (c *chat) render(params json.RawMessage) {
	var e cli.Event
	if err := json.Unmarshal(params, &e); err != nil {
		return
	}

	switch e.Type {
	case string(sessionevent.NotifyMessage):
		c.recordOutput(e.Generation)
		c.clearActivity()
		c.println(e.Message)
	case "waiting":
		c.recordOutput(e.Generation)
		c.clearActivity()
		c.println(e.Message)
	case "session_opened", "session_replaced", "session_closed":
		c.applyLifecycle(e.Generation, e.OldSessionID, e.SessionID, e.Type)
	case string(sessionevent.NotifyHeartbeat):
		c.showActivity()
	case string(sessionevent.NotifyStateChanged):
		if e.Status == "idle" && c.outputDelivered(e.AfterOutputID) {
			c.clearActivity()
			c.setBusy(false)
			c.prompt()
		}
	default:
	}
}

// showActivity draws the one-line "still working" marker on the heartbeat the
// loop emits each iteration, so a long turn does not look like a hang.
func (c *chat) showActivity() {
	c.mu.Lock()
	c.activity++
	shown := c.activity
	c.mu.Unlock()

	c.printf("\r  working… %ds ", shown)
}

// clearActivity wipes the marker before anything else writes over it.
func (c *chat) clearActivity() {
	c.mu.Lock()
	drawn := c.activity > 0
	c.activity = 0
	c.mu.Unlock()

	if drawn {
		c.write("\r\033[K")
	}
}
