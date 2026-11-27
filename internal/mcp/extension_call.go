package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
)

func (c *Client) extensionCall(ctx context.Context, method string, params map[string]any, result any) error {
	ctx, cancel := context.WithTimeout(ctx, initTimeout)
	defer cancel()

	if mcp.IsModernProtocol(c.protocolVersion) {
		params["_meta"] = map[string]any{
			mcp.MetaKeyProtocolVersion:    c.protocolVersion,
			mcp.MetaKeyClientInfo:         clientInfo,
			mcp.MetaKeyClientCapabilities: mcp.ClientCapabilities{},
		}
	}

	response, err := c.transport.SendRequest(ctx, transport.JSONRPCRequest{
		JSONRPC: mcp.JSONRPC_VERSION,
		ID:      mcp.NewRequestId("coagent-" + strconv.FormatInt(c.extensionIDs.Add(1), 10)),
		Method:  method,
		Params:  params,
	})
	if err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}

	if response.Error != nil {
		return fmt.Errorf("%s: %w", method, response.Error.AsError())
	}

	if err := json.Unmarshal(response.Result, result); err != nil {
		return fmt.Errorf("%s: %w", method, err)
	}

	return nil
}
