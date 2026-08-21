package lsp

import (
	"context"
	"encoding/json"
	"fmt"
)

// call sends a request and waits for response, bounded by lspCallTimeout so a
// wedged server fails on a deadline instead of hanging the tool and the loop.
func (c *client) call(ctx context.Context, method string, params, result any) error {
	ctx, cancel := context.WithTimeout(ctx, lspCallTimeout)
	defer cancel()

	id, data, err := c.requestFrame(method, params)
	if err != nil {
		return err
	}

	response := make(chan rpcResult, 1)

	c.pendingMu.Lock()
	c.pending[id] = response
	c.pendingMu.Unlock()

	if err := c.send(ctx, data); err != nil {
		c.removePending(id)
		return fmt.Errorf("send request: %w", err)
	}

	return c.awaitResponse(ctx, id, response, result)
}

func (c *client) requestFrame(method string, params any) (int64, []byte, error) {
	paramsBytes, err := json.Marshal(params)
	if err != nil {
		return 0, nil, fmt.Errorf("marshal params: %w", err)
	}

	id := c.idGen.Add(1)

	data, err := json.Marshal(Request{JSONRPC: jsonRPCVersion, ID: id, Method: method, Params: paramsBytes})
	if err != nil {
		return 0, nil, fmt.Errorf("marshal request: %w", err)
	}

	return id, data, nil
}

func (c *client) awaitResponse(ctx context.Context, id int64, response chan rpcResult, result any) error {
	select {
	case received := <-response:
		if received.err != nil {
			return received.err
		}

		if result == nil {
			return nil
		}

		if err := json.Unmarshal(received.result, result); err != nil {
			return fmt.Errorf("unmarshal result: %w", err)
		}

		return nil
	case <-ctx.Done():
		if c.removePending(id) {
			c.cancelRequest(ctx, id)
		}

		return ctx.Err()
	}
}
