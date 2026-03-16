package ctl

import "encoding/json"

// ProtocolVersion is the RPC contract version, bumped when an op's shape changes
// incompatibly. Skew between a client and the daemon is a warning surfaced in
// UIs, never a refusal — a mismatched CLI that can still read `status` is more
// useful than one that will not connect.
const ProtocolVersion = 1

// jsonrpcVersion is the only value the version field ever carries.
const jsonrpcVersion = "2.0"

// AppName identifies the greeting's speaker, so a client that dialled the wrong
// socket learns it from the first line instead of from a parse error.
const AppName = "coagent"

// JSON-RPC error codes. The reserved range is used as the spec defines it;
// everything above -32000 is ours.
const (
	CodeParse          = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternal       = -32603
	// CodeStarting answers every op while the daemon is still assembling its
	// control plane, so "booting" never reaches a client as "unknown method".
	CodeStarting = -32000
)

// Op names. They are snake_case, and they are the contract — docs/control-api.md
// documents each one's params and result.
const (
	OpStatus        = "status"
	OpSetProvider   = "set_provider"
	OpSetSecret     = "set_secret"
	OpRestartDaemon = "restart_daemon"
)

// Greeting is the unsolicited first line the server writes on connect. Reading
// it is how a client learns the daemon's version without spending a round trip,
// and connecting at all is the liveness check.
type Greeting struct {
	App             string `json:"app"`
	BinaryVersion   string `json:"binary_version"`
	ProtocolVersion int    `json:"protocol_version"`
}

type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Response is a JSON-RPC 2.0 response. Exactly one of Result and Error is set.
//
// A rejected mutation is a *successful* response carrying a verdict with
// applied=false. Error is reserved for transport and malformed requests, so a
// client can always tell "your input was wrong" from "the daemon is broken".
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Notification is a server→client push — a JSON-RPC notification, meaning a
// request with no id. Chat output and secret prompts ride these, on the same
// connection as responses.
type Notification struct {
	JSONRPC string          `json:"jsonrpc"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return e.Message }

// frame is one inbound line on the client side. Responses and pushes share the
// connection, so the client tells them apart by shape rather than by order: a
// response carries an id, a notification carries a method and none.
type frame struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
}

// isResponse reports whether the frame answers a request. A JSON `null` id is
// the spec's "could not tell which request", which is still not a push.
func (f frame) isResponse() bool {
	return len(f.ID) > 0 && string(f.ID) != "null"
}
