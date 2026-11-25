package tool

import (
	"context"
	"encoding/json"
)

// HookName aliases must never equal another tool's native ID: that ID wins.
type HookName struct {
	Name    string
	Aliases []string
}

type HookNamed interface { //nolint:iface // Declared here, implemented by tools elsewhere.
	Tool
	HookName() HookName
}

type HookCall struct {
	ToolID string
	Name   HookName
	Input  json.RawMessage
}

type HookProjector interface {
	Tool
	HookCalls(context.Context, json.RawMessage) ([]HookCall, error)
}

type HookResponder interface { //nolint:iface // Declared here, implemented by tools elsewhere.
	Tool
	HookResponse(*Result) any
}

type HookVerdict struct {
	Refusal string
	Context []string
}

type CallHooks interface {
	BeforeCall(context.Context, Call) HookVerdict
	AfterCall(context.Context, Call, *Result) []string
}

type callHooksKey struct{}

func WithCallHooks(ctx context.Context, hooks CallHooks) context.Context {
	return context.WithValue(ctx, callHooksKey{}, hooks)
}

func callHooksFromContext(ctx context.Context) CallHooks {
	hooks, _ := ctx.Value(callHooksKey{}).(CallHooks)
	return hooks
}
