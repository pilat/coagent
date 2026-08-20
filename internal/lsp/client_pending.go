package lsp

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

func validServerRequestID(id json.RawMessage) bool {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(id)))
	decoder.UseNumber()

	if decoder.Decode(&value) != nil {
		return false
	}

	switch typed := value.(type) {
	case string:
		return true
	case json.Number:
		_, err := typed.Int64()
		return err == nil
	default:
		return false
	}
}

func (c *client) cleanupPending() {
	c.exited.Store(true)
	c.pendingMu.Lock()
	for id, ch := range c.pending {
		ch <- rpcResult{err: ErrClientExited}

		delete(c.pending, id)
	}
	c.pendingMu.Unlock()

	c.exitOnce.Do(func() {
		if c.onExit != nil {
			c.onExit()
		}
	})
}

func (c *client) hasExited() bool { return c.exited.Load() }

func (c *client) completePending(id int64, result rpcResult) bool {
	c.pendingMu.Lock()

	ch, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.pendingMu.Unlock()

	if !ok {
		return false
	}

	ch <- result

	return true
}

func (c *client) removePending(id int64) bool {
	c.pendingMu.Lock()
	_, ok := c.pending[id]
	delete(c.pending, id)
	c.pendingMu.Unlock()

	return ok
}

func (c *client) cancelRequest(parent context.Context, id int64) {
	go func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), time.Second)
		defer cancel()

		_ = c.notify(ctx, "$/cancelRequest", map[string]int64{"id": id})
	}()
}
