package main

import (
	"context"
	"encoding/json"

	"github.com/pilat/coagent/internal/ctl"
	"github.com/pilat/coagent/internal/managers/cli"
	"github.com/pilat/coagent/internal/sessionevent"
)

// startEvents hands the current connection to one push reader. Exactly one runs
// at a time; reconnect waits for the previous one before asking for another.
func (c *chat) startEvents(ctx context.Context) {
	client := c.currentClient()
	if client == nil {
		return
	}

	c.pushWG.Add(1)

	// Counted before the goroutine runs, so a caller that just started a reader
	// can be held to the one-owner rule immediately.
	if live := c.pushLive.Add(1); live > c.pushPeak.Load() {
		c.pushPeak.Store(live)
	}

	go func() {
		defer func() {
			c.pushLive.Add(-1)
			c.pushWG.Done()
		}()

		c.printEvents(ctx, client)
	}()
}

// printEvents renders the push stream of one connection. It exits when that
// connection drops; the send path is what notices and reconnects.
func (c *chat) printEvents(ctx context.Context, client *ctl.Client) {
	for {
		select {
		case <-ctx.Done():
			return
		case n, ok := <-client.Notifications():
			if !ok {
				return
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

	select {
	case c.secrets <- req:
	default:
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
		c.clearActivity()
		c.println(e.Message)
	case string(sessionevent.NotifyHeartbeat):
		c.showActivity()
	case string(sessionevent.NotifyStateChanged):
		if e.Status == "idle" {
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
