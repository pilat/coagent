package lsp

import (
	"context"
	"encoding/json"
	"path/filepath"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

func (c *client) handleServerRequest(ctx context.Context, id json.RawMessage, method string, params json.RawMessage) {
	var result any
	var rpcErr *RPCError

	switch method {
	case "workspace/configuration":
		var request struct {
			Items []json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal(params, &request); err != nil {
			rpcErr = &RPCError{Code: -32600, Message: jsonRPCInvalidRequest}
			break
		}

		result = make([]any, len(request.Items))
	case "client/registerCapability", "client/unregisterCapability", "window/workDoneProgress/create":
		result = nil
	case "workspace/workspaceFolders":
		result = []map[string]string{{"uri": fileURI(c.rootPath), "name": filepath.Base(c.rootPath)}}
	default:
		rpcErr = &RPCError{Code: -32601, Message: "Method not found"}
	}

	c.respondServerRequest(ctx, id, result, rpcErr)
}

func (c *client) respondServerRequest(ctx context.Context, id json.RawMessage, result any, rpcErr *RPCError) {
	response := struct {
		JSONRPC string           `json:"jsonrpc"`
		ID      json.RawMessage  `json:"id"`
		Result  *json.RawMessage `json:"result,omitempty"`
		Error   *RPCError        `json:"error,omitempty"`
	}{JSONRPC: jsonRPCVersion, ID: id, Error: rpcErr}
	if rpcErr == nil {
		data, err := json.Marshal(result)
		if err != nil {
			logger.Ctx(ctx).Debug("marshal LSP server request result", zap.Error(err))
			return
		}

		raw := json.RawMessage(data)
		response.Result = &raw
	}

	data, err := json.Marshal(response)
	if err != nil {
		logger.Ctx(ctx).Debug("marshal LSP server request response", zap.Error(err))
		return
	}

	if err := c.send(ctx, data); err != nil {
		logger.Ctx(ctx).Debug("respond to LSP server request", zap.Error(err))
	}
}
