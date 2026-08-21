package lsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/pilat/coagent/internal/logger"
)

const (
	maxLSPHeaderLine      = 4 << 10
	maxLSPHeaderSize      = 16 << 10
	maxLSPFrameSize       = 16 << 20
	jsonRPCInvalidRequest = "Invalid Request"
)

var errLSPProtocol = errors.New("LSP protocol error")

type protocolError struct{ message string }

func (e *protocolError) Error() string { return e.message }

func (e *protocolError) Is(target error) bool { return target == errLSPProtocol }

func (c *client) readLoop(ctx context.Context) {
	defer func() {
		if recovered := recover(); recovered != nil {
			logger.Ctx(ctx).Error("LSP reader panic", zap.Any("recovered", recovered), zap.Stack("stack"))
		}

		c.handleReaderExit()
	}()

	for {
		body, err := readLSPFrame(c.reader)
		if err != nil {
			logger.Ctx(ctx).Debug("LSP reader stopped", zap.Error(err))
			return
		}

		c.routeFrame(ctx, body)
	}
}

func (c *client) handleReaderExit() {
	c.failWriter()
}

func readLSPFrame(reader *bufio.Reader) ([]byte, error) {
	contentLength, err := readLSPHeaders(reader)
	if err != nil {
		return nil, err
	}

	body := make([]byte, contentLength)
	if _, err := io.ReadFull(reader, body); err != nil {
		return nil, fmt.Errorf("read LSP body: %w", err)
	}

	return body, nil
}

func readLSPHeaders(reader *bufio.Reader) (int, error) {
	contentLength := -1
	total := 0

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return 0, fmt.Errorf("read LSP header: %w", err)
		}

		total += len(line)
		if len(line) > maxLSPHeaderLine || total > maxLSPHeaderSize {
			return 0, &protocolError{message: "LSP headers exceed limit"}
		}

		if !strings.HasSuffix(line, "\r\n") {
			return 0, &protocolError{message: "LSP headers must use CRLF"}
		}

		if line == "\r\n" {
			break
		}

		contentLength, err = parseContentLength(line, contentLength)
		if err != nil {
			return 0, err
		}
	}

	if contentLength < 0 {
		return 0, &protocolError{message: "missing LSP Content-Length"}
	}

	return contentLength, nil
}

func parseContentLength(line string, current int) (int, error) {
	name, value, ok := strings.Cut(strings.TrimSuffix(line, "\r\n"), ":")
	if !ok || name == "" {
		return 0, &protocolError{message: "invalid LSP header"}
	}

	if !strings.EqualFold(name, "Content-Length") {
		return current, nil
	}

	if current >= 0 {
		return 0, &protocolError{message: "duplicate LSP Content-Length"}
	}

	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed < 1 || parsed > maxLSPFrameSize {
		return 0, &protocolError{message: "invalid LSP Content-Length"}
	}

	return parsed, nil
}

func (c *client) routeFrame(ctx context.Context, body []byte) {
	var frame map[string]json.RawMessage
	if err := json.Unmarshal(body, &frame); err != nil {
		logger.Ctx(ctx).Debug("invalid LSP JSON frame", zap.Error(err))
		return
	}

	if !hasJSONRPCVersion(frame) {
		c.handleInvalidJSONRPCFrame(ctx, frame)
		return
	}

	if isServerMethodFrame(frame) {
		c.routeServerMethod(ctx, frame)
		return
	}

	c.routeResponse(ctx, frame)
}

func hasJSONRPCVersion(frame map[string]json.RawMessage) bool {
	var version string

	return json.Unmarshal(frame["jsonrpc"], &version) == nil && version == jsonRPCVersion
}

func (c *client) handleInvalidJSONRPCFrame(ctx context.Context, frame map[string]json.RawMessage) {
	id, hasID := frame["id"]
	_, isMethod := frame["method"]

	if isMethod && hasID && validServerRequestID(id) {
		c.respondServerRequest(ctx, id, nil, &RPCError{Code: -32600, Message: jsonRPCInvalidRequest})
		return
	}

	if !isMethod && hasID {
		if numericID, ok := numericResponseID(id); ok {
			c.completePending(numericID, rpcResult{err: &protocolError{message: "invalid LSP JSON-RPC version"}})
		}
	}

	logger.Ctx(ctx).Debug("invalid LSP JSON-RPC version")
}

func isServerMethodFrame(frame map[string]json.RawMessage) bool {
	_, ok := frame["method"]

	return ok
}

func (c *client) routeServerMethod(ctx context.Context, frame map[string]json.RawMessage) {
	id, hasID := frame["id"]

	var method string
	if err := json.Unmarshal(frame["method"], &method); err != nil || method == "" {
		if hasID && validServerRequestID(id) {
			c.respondServerRequest(ctx, id, nil, &RPCError{Code: -32600, Message: "Invalid Request"})
		}

		logger.Ctx(ctx).Debug("invalid LSP server method", zap.Error(err))

		return
	}

	if !hasID {
		c.handleNotification(ctx, &Notification{JSONRPC: jsonRPCVersion, Method: method, Params: frame["params"]})
		return
	}

	if !validServerRequestID(id) {
		logger.Ctx(ctx).Debug("invalid LSP server request ID")
		return
	}

	c.handleServerRequest(ctx, id, method, frame["params"])
}

func (c *client) routeResponse(ctx context.Context, frame map[string]json.RawMessage) {
	idRaw, hasID := frame["id"]
	result, hasResult := frame["result"]
	errorRaw, hasError := frame["error"]

	if !hasID {
		logger.Ctx(ctx).Debug("invalid LSP response without ID")
		return
	}

	id, ok := numericResponseID(idRaw)
	if !ok {
		logger.Ctx(ctx).Debug("invalid LSP response ID")
		return
	}

	if hasResult == hasError {
		if !c.completePending(
			id,
			rpcResult{err: &protocolError{message: "LSP response must contain exactly one of result or error"}},
		) {
			logger.Ctx(ctx).Debug("unknown LSP response ID", zap.Int64("id", id))
		}

		return
	}

	if hasError {
		rpcErr, err := decodeRPCError(errorRaw)
		if err != nil {
			_ = c.completePending(id, rpcResult{err: &protocolError{message: "invalid LSP response error"}})
			return
		}

		_ = c.completePending(id, rpcResult{err: rpcErr})

		return
	}

	if !c.completePending(id, rpcResult{result: result}) {
		logger.Ctx(ctx).Debug("unknown LSP response ID", zap.Int64("id", id))
	}
}

func decodeRPCError(raw json.RawMessage) (*RPCError, error) {
	var wire struct {
		Code    json.RawMessage `json:"code"`
		Message json.RawMessage `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("unmarshal LSP error object: %w", err)
	}

	if len(wire.Code) == 0 || bytes.Equal(bytes.TrimSpace(wire.Code), []byte("null")) ||
		len(wire.Message) == 0 || bytes.Equal(bytes.TrimSpace(wire.Message), []byte("null")) {
		return nil, &protocolError{message: "invalid LSP response error"}
	}

	var rpcErr RPCError
	if err := json.Unmarshal(wire.Code, &rpcErr.Code); err != nil {
		return nil, fmt.Errorf("unmarshal LSP error code: %w", err)
	}

	if err := json.Unmarshal(wire.Message, &rpcErr.Message); err != nil {
		return nil, fmt.Errorf("unmarshal LSP error message: %w", err)
	}

	rpcErr.Data = wire.Data

	return &rpcErr, nil
}

func numericResponseID(raw json.RawMessage) (int64, bool) {
	var number json.Number
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()

	if err := decoder.Decode(&number); err != nil {
		return 0, false
	}

	id, err := number.Int64()

	return id, err == nil
}
